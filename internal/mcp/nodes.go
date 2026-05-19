package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// RegisterNodes registers the create_nodes and add_observations tools on the
// server. limiter enforces per-IP write-tool rate limits on both tools.
//
// Soft-delete operations (formerly delete_nodes, delete_observations) are not
// exposed on the MCP endpoint — the endpoint is unauthenticated and destructive
// ops on a public surface are a security risk. Use Supabase Studio or the
// store.MarkDeletedByNames / store.MarkDeletedByNodeAndTexts admin functions.
func RegisterNodes(s *server.MCPServer, pool *pgxpool.Pool, limiter *ratelimit.Limiter) {
	s.AddTool(
		mcplib.NewTool("create_nodes",
			mcplib.WithDescription("Create new nodes in the knowledge graph. Each node must have name, nodeType, and observations. Idempotent on name."),
			mcplib.WithArray("nodes", mcplib.Required()),
		),
		createNodesHandler(pool, limiter),
	)

	s.AddTool(
		mcplib.NewTool("add_observations",
			mcplib.WithDescription("Add observations to existing nodes. Each item must have nodeName and contents (array of strings)."),
			mcplib.WithArray("observations", mcplib.Required()),
		),
		addObservationsHandler(pool, limiter),
	)
}

// ── create_nodes ──────────────────────────────────────────────────────────────

type createNodeInput struct {
	Name         string   `json:"name"`
	NodeType     string   `json:"nodeType"`
	Observations []string `json:"observations"`
}

type createNodesArgs struct {
	Nodes []createNodeInput `json:"nodes"`
}

func createNodesHandler(pool *pgxpool.Pool, limiter *ratelimit.Limiter) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if result := checkRateLimit(ctx, limiter); result != nil {
			return result, nil
		}

		var args createNodesArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		// Build validate.Payload and run the Content Filter before opening any Tx.
		vp := validate.Payload{Nodes: make([]validate.Node, len(args.Nodes))}
		for i, n := range args.Nodes {
			vp.Nodes[i] = validate.Node{
				Name:         n.Name,
				NodeType:     n.NodeType,
				Observations: n.Observations,
			}
		}
		if verr := validate.Run(&vp, validate.KindNodes); verr != nil {
			return verr.ToMCPResult(), nil
		}

		userID, email := attributionFromContext(ctx)
		createdNodes, createdObs, err := execCreateNodes(ctx, pool, args.Nodes, userID, email)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{
			"created_nodes":        createdNodes,
			"created_observations": createdObs,
		}), nil
	}
}

func execCreateNodes(ctx context.Context, pool *pgxpool.Pool, nodes []createNodeInput, userID, email *string) (int, int, error) {
	// Collect all observation texts for batch embedding before opening any Tx.
	// This ensures a model failure does not waste a DB transaction.
	var allObs []obsRef
	for ni, n := range nodes {
		for oi, text := range n.Observations {
			truncated := embed.TruncateToTokens(text, 256)
			nodes[ni].Observations[oi] = truncated
			allObs = append(allObs, obsRef{nodeIdx: ni, obsIdx: oi, text: truncated})
		}
	}

	embeddingsByObs, err := batchEmbed(ctx, allObs)
	if err != nil {
		return 0, 0, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	createdNodes, createdObs := 0, 0
	for ni, n := range nodes {
		id, err := store.Create(ctx, tx, n.Name, n.NodeType, userID, email)
		if err != nil {
			return 0, 0, fmt.Errorf("create node %q: %w", n.Name, err)
		}
		createdNodes++

		for oi, obsText := range n.Observations {
			vec := embeddingsByObs[[2]int{ni, oi}]
			_, inserted, err := store.Insert(ctx, tx, id, obsText, vec, userID, email)
			if err != nil {
				return 0, 0, fmt.Errorf("insert observation for %q: %w", n.Name, err)
			}
			if inserted {
				createdObs++
			}
		}
	}

	return createdNodes, createdObs, tx.Commit(ctx)
}

// obsRef identifies a single observation by its position in the input batch.
type obsRef struct {
	nodeIdx int
	obsIdx  int
	text    string
}

// batchEmbed encodes all observation texts in one call and returns a map
// keyed by [nodeIdx, obsIdx] → pgvector.Vector. A model failure is returned
// as an error — callers must not proceed with DB writes if encoding fails.
func batchEmbed(ctx context.Context, refs []obsRef) (map[[2]int]pgvector.Vector, error) {
	result := make(map[[2]int]pgvector.Vector, len(refs))
	if len(refs) == 0 {
		return result, nil
	}

	texts := make([]string, len(refs))
	for i, r := range refs {
		texts[i] = r.text
	}

	vecs, err := embed.Default().Encode(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("embed encode: %w", err)
	}

	for i, r := range refs {
		result[[2]int{r.nodeIdx, r.obsIdx}] = pgvector.NewVector(vecs[i])
	}
	return result, nil
}

// ── add_observations ──────────────────────────────────────────────────────────

type addObsInput struct {
	NodeName string   `json:"nodeName"`
	Contents []string `json:"contents"`
}

type addObservationsArgs struct {
	Observations []addObsInput `json:"observations"`
}

func addObservationsHandler(pool *pgxpool.Pool, limiter *ratelimit.Limiter) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if result := checkRateLimit(ctx, limiter); result != nil {
			return result, nil
		}

		var args addObservationsArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		// Flatten into validate.Observation slice for the Content Filter.
		// vp2.Observations may be mutated (secrets redacted) when SECRET_MODE=redact.
		var flatObs []validate.Observation
		for _, item := range args.Observations {
			for _, text := range item.Contents {
				flatObs = append(flatObs, validate.Observation{NodeName: item.NodeName, Text: text})
			}
		}
		vp2 := validate.Payload{Observations: flatObs}
		if verr := validate.Run(&vp2, validate.KindObservations); verr != nil {
			return verr.ToMCPResult(), nil
		}

		// Propagate potentially-redacted texts back into args so the DB write
		// stores the scrubbed version rather than the original.
		flatIdx := 0
		for ii := range args.Observations {
			for ci := range args.Observations[ii].Contents {
				args.Observations[ii].Contents[ci] = vp2.Observations[flatIdx].Text
				flatIdx++
			}
		}

		userID, email := attributionFromContext(ctx)
		added, err := execAddObservations(ctx, pool, args.Observations, userID, email)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{"added": added}), nil
	}
}

func execAddObservations(ctx context.Context, pool *pgxpool.Pool, items []addObsInput, userID, email *string) (int, error) {
	// Collect and truncate all texts; batch-encode before opening any Tx.
	var allObs []obsRef
	for ii, item := range items {
		for oi, text := range item.Contents {
			truncated := embed.TruncateToTokens(text, 256)
			items[ii].Contents[oi] = truncated
			allObs = append(allObs, obsRef{nodeIdx: ii, obsIdx: oi, text: truncated})
		}
	}

	embeddingsByObs, err := batchEmbed(ctx, allObs)
	if err != nil {
		return 0, err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	added := 0
	for ii, item := range items {
		id, _, found, err := store.FindByName(ctx, tx, item.NodeName)
		if err != nil {
			return 0, fmt.Errorf("find node %q: %w", item.NodeName, err)
		}
		if !found {
			return 0, &nodeNotFoundError{name: item.NodeName}
		}

		for oi, text := range item.Contents {
			vec := embeddingsByObs[[2]int{ii, oi}]
			_, inserted, err := store.Insert(ctx, tx, id, text, vec, userID, email)
			if err != nil {
				return 0, fmt.Errorf("insert observation for %q: %w", item.NodeName, err)
			}
			if inserted {
				added++
			}
		}
	}

	return added, tx.Commit(ctx)
}

// ── shared helpers ────────────────────────────────────────────────────────────

// attributionFromContext extracts the optional user attribution from ctx.
// Returns (nil, nil) when no user is authenticated (stdio path, MCP_AUTH=none),
// which maps directly to SQL NULL on the nullable attribution columns.
func attributionFromContext(ctx context.Context) (userID, email *string) {
	if sub := auth.UserIDFromContext(ctx); sub != "" {
		userID = &sub
	}
	if em := auth.EmailFromContext(ctx); em != "" {
		email = &em
	}
	return userID, email
}

// nodeNotFoundError is returned by add_observations when a node name cannot be
// resolved. It is treated as a non-policy error (no Content Filter involvement)
// and surfaces as a structured JSON error result.
type nodeNotFoundError struct {
	name string
}

func (e *nodeNotFoundError) Error() string {
	return fmt.Sprintf("node not found: %s", e.name)
}

// checkRateLimit reads the rate-limit key from ctx and checks the write-tool
// budget. Key selection priority:
//  1. sub claim (JWT-authenticated HTTP request) — per-user bucket.
//  2. IP address (unauthenticated HTTP request) — per-IP bucket.
//  3. Neither present (stdio transport) — use the process-wide stdio bucket.
//
// Returns a non-nil *mcp.CallToolResult (IsError=true) when the caller is
// over quota, or nil when the call is allowed to proceed.
func checkRateLimit(ctx context.Context, limiter *ratelimit.Limiter) *mcplib.CallToolResult {
	if limiter == nil {
		return nil
	}

	// Prefer sub over IP for HTTP requests so per-user budgets are independent
	// of IP sharing (e.g. multiple users behind the same NAT gateway).
	key := ratelimit.SubFromContext(ctx)
	if key == "" {
		key = ratelimit.IPFromContext(ctx)
	}

	if key == "" {
		// stdio transport — check the process-wide stdio bucket instead.
		bucket := ratelimit.InitStdio()
		allowed, retryAfter := bucket.Allow()
		if allowed {
			return nil
		}
		return rateLimitResult(int(math.Ceil(retryAfter.Seconds())))
	}

	allowed, retryAfter := limiter.Allow(key)
	if allowed {
		return nil
	}
	return rateLimitResult(int(math.Ceil(retryAfter.Seconds())))
}

// rateLimitResult returns a structured rate-limit error result.
func rateLimitResult(retrySecs int) *mcplib.CallToolResult {
	payload, _ := json.Marshal(map[string]any{
		"code":                validate.CodeRateLimited,
		"message":             fmt.Sprintf("Demasiadas escrituras. Reintentar en %d segundos.", retrySecs),
		"layer":               validate.LayerRateLimit,
		"retry_after_seconds": retrySecs,
	})
	return &mcplib.CallToolResult{
		IsError: true,
		Content: []mcplib.Content{
			mcplib.TextContent{Type: mcplib.ContentTypeText, Text: string(payload)},
		},
	}
}

// jsonResult marshals v and returns a successful TextContent result.
func jsonResult(v any) *mcplib.CallToolResult {
	data, _ := json.Marshal(v)
	return mcplib.NewToolResultText(string(data))
}

// errorResult returns a non-policy error as a TextContent result with IsError=true.
func errorResult(msg string) *mcplib.CallToolResult {
	data, _ := json.Marshal(map[string]string{"error": msg})
	return &mcplib.CallToolResult{
		Content: []mcplib.Content{
			mcplib.TextContent{
				Type: mcplib.ContentTypeText,
				Text: string(data),
			},
		},
		IsError: true,
	}
}
