package mcp

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

const (
	// suggestionsTopK is the maximum number of suggestions returned.
	// Actual count is min(suggestionsTopK, centroids_computed).
	suggestionsTopK = 3

	// centroidCacheTTL bounds how stale the cached centroids may get. The next
	// call after expiry recomputes; in-flight calls use the cached value.
	// Balances freshness vs. DB load on busy servers.
	centroidCacheTTL = 5 * time.Minute

	// maxInputText caps the input length so a pathological prompt cannot trigger
	// an oversized embedding request. The model truncates to 256 tokens internally
	// but we want a fast pre-flight reject at the MCP boundary.
	maxInputText = 8192
)

// ── centroid cache ────────────────────────────────────────────────────────────

// centroidCache holds per-project centroids indexed by project key.
// Key "" means all-projects; any other string is a project-scoped cache entry.
type centroidCache struct {
	mu        sync.RWMutex
	byProject map[string]centroidEntry
}

type centroidEntry struct {
	centroids  map[string][]float32
	computedAt time.Time
}

func newCentroidCache() *centroidCache {
	return &centroidCache{byProject: make(map[string]centroidEntry)}
}

func (c *centroidCache) get(key string) (map[string][]float32, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.byProject[key]
	if !ok {
		return nil, false
	}
	if time.Since(e.computedAt) > centroidCacheTTL {
		return nil, false
	}
	return e.centroids, true
}

func (c *centroidCache) put(key string, centroids map[string][]float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byProject[key] = centroidEntry{centroids: centroids, computedAt: time.Now()}
}

// ── wire types ────────────────────────────────────────────────────────────────

type suggestNodeTypeArgs struct {
	Text    string `json:"text"`
	Project string `json:"project,omitempty"`
}

type nodeTypeSuggestion struct {
	NodeType string  `json:"node_type"`
	Score    float64 `json:"score"`
}

type suggestStats struct {
	CentroidsComputed int      `json:"centroids_computed"`
	TypesUnseen       []string `json:"types_unseen"`
}

type suggestResponse struct {
	Suggestions []nodeTypeSuggestion `json:"suggestions"`
	Stats       suggestStats         `json:"stats"`
}

// ── handler ───────────────────────────────────────────────────────────────────

// suggestNodeTypeHandler returns a ToolHandlerFunc for the suggest_node_type tool.
// The centroid cache is closure-scoped so each server instance maintains its own
// independent TTL budget.
func suggestNodeTypeHandler(pool *pgxpool.Pool, cache *centroidCache) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		args, verr := parseSuggestArgs(req)
		if verr != nil {
			return verr, nil
		}

		centroids, err := loadCentroids(ctx, pool, cache, args.Project)
		if err != nil {
			return errorResult("centroids: " + err.Error()), nil
		}

		if len(centroids) == 0 {
			return jsonResult(buildEmptySuggestResponse()), nil
		}

		suggestions, err := scoreCentroids(ctx, args.Text, centroids)
		if err != nil {
			return errorResult("embed: " + err.Error()), nil
		}

		return jsonResult(buildSuggestResponse(suggestions, centroids)), nil
	}
}

// parseSuggestArgs validates and normalises the tool arguments. Returns the
// args or a ready-to-return *mcp.CallToolResult on any validation failure.
func parseSuggestArgs(req mcplib.CallToolRequest) (suggestNodeTypeArgs, *mcplib.CallToolResult) {
	var args suggestNodeTypeArgs
	if err := req.BindArguments(&args); err != nil {
		return args, errorResult("invalid arguments: " + err.Error())
	}

	args.Text = strings.TrimSpace(args.Text)
	if args.Text == "" {
		return args, errorResult("text must be non-empty")
	}
	if len(args.Text) > maxInputText {
		return args, errorResult("text exceeds 8192 chars")
	}

	if args.Project != "" {
		if verr := validate.Check(args.Project); verr != nil {
			return args, verr.ToMCPResult()
		}
	}

	return args, nil
}

// loadCentroids fetches centroids from the cache or recomputes them from DB.
// cacheKey is the project string ("" for all-projects).
func loadCentroids(ctx context.Context, pool *pgxpool.Pool, cache *centroidCache, project string) (map[string][]float32, error) {
	if centroids, ok := cache.get(project); ok {
		return centroids, nil
	}

	pf := projectFilterFrom(project)
	centroids, err := store.CentroidsByType(ctx, pool, pf)
	if err != nil {
		return nil, err
	}

	cache.put(project, centroids)
	return centroids, nil
}

// scoreCentroids embeds the query text and computes cosine similarity against
// each centroid, returning up to suggestionsTopK results sorted by score desc.
func scoreCentroids(ctx context.Context, text string, centroids map[string][]float32) ([]nodeTypeSuggestion, error) {
	vecs, err := embed.Default().Encode(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	query := vecs[0]

	scores := make([]nodeTypeSuggestion, 0, len(centroids))
	for nodeType, centroid := range centroids {
		scores = append(scores, nodeTypeSuggestion{
			NodeType: nodeType,
			Score:    cosineSim(query, centroid),
		})
	}

	sort.Slice(scores, func(i, j int) bool { return scores[i].Score > scores[j].Score })
	if len(scores) > suggestionsTopK {
		scores = scores[:suggestionsTopK]
	}
	return scores, nil
}

// buildSuggestResponse assembles the full response including types_unseen.
func buildSuggestResponse(suggestions []nodeTypeSuggestion, centroids map[string][]float32) suggestResponse {
	seen := make(map[string]struct{}, len(centroids))
	for t := range centroids {
		seen[t] = struct{}{}
	}

	unseen := make([]string, 0)
	for _, t := range validate.SortedNodeTypes() {
		if _, ok := seen[t]; !ok {
			unseen = append(unseen, t)
		}
	}

	return suggestResponse{
		Suggestions: suggestions,
		Stats: suggestStats{
			CentroidsComputed: len(centroids),
			TypesUnseen:       unseen,
		},
	}
}

// buildEmptySuggestResponse returns the correct wire shape for an empty corpus.
func buildEmptySuggestResponse() suggestResponse {
	return suggestResponse{
		Suggestions: []nodeTypeSuggestion{},
		Stats: suggestStats{
			CentroidsComputed: 0,
			TypesUnseen:       validate.SortedNodeTypes(),
		},
	}
}

// cosineSim computes cosine similarity between two float32 vectors.
// Returns 0 when either vector is zero-length or all-zeros.
func cosineSim(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ── registration ─────────────────────────────────────────────────────────────

// RegisterSuggestNodeType registers the suggest_node_type tool on s.
// A fresh centroid cache is created per registration — its TTL is scoped to the
// lifetime of the server process, so restarts flush the cache automatically.
func RegisterSuggestNodeType(s *server.MCPServer, pool *pgxpool.Pool) {
	tool := mcplib.NewTool("suggest_node_type",
		mcplib.WithDescription("Returns the top-3 most likely nodeType values for a given text, "+
			"based on semantic similarity to per-type centroids computed from active observations. "+
			"Read-only — no rate limit, no content filter."),
		mcplib.WithString("text",
			mcplib.Required(),
			mcplib.Description("Free-form text to classify. Max 8192 chars."),
		),
		mcplib.WithString("project",
			mcplib.Description("Optional project ID to scope the centroids. "+
				"When omitted, centroids span all projects."),
		),
	)
	s.AddTool(tool, suggestNodeTypeHandler(pool, newCentroidCache()))
}

