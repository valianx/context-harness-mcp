// Package viewer serves an authenticated single-page web UI for browsing the
// knowledge graph. It registers four routes on the caller's mux:
//
//   - GET /viewer/              — serves the embedded HTML page (session-gated)
//   - GET /viewer/api/search    — returns JSON (session-gated)
//   - GET /viewer/api/projects  — returns the list of distinct project IDs (session-gated)
//   - GET /viewer/api/node-types — returns the static list of valid node types (session-gated)
//
// Access requires a valid ch_session cookie (AC-9 — STAGE-GATE-1 override of the
// v0.2.0 "viewer stays public read-only" convention in CLAUDE.md §5).
// HTML paths (/viewer/, non-api) → 302 /auth/login?next=<path> on missing/invalid session.
// JSON paths (/viewer/api/*) → 401 {"error":"authentication required"}.
//
// Search alignment: the viewer's search endpoint (/viewer/api/search?q=...) mirrors the
// MCP search_nodes tool: top 10 results ranked by cosine similarity. Operators see exactly
// what an agent sees when it calls search_nodes with the same query. The list-all endpoint
// (no q) returns up to 50 nodes for browsing.
package viewer

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"

	embedpkg "github.com/mariogutierrez/context-harness-mcp/internal/embed"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
	"github.com/mariogutierrez/context-harness-mcp/internal/web"
)

//go:embed templates/*
var templateFS embed.FS

// Register adds the viewer routes to mux. pool must be non-nil — the search
// and projects handlers require DB access. The node-types handler is static.
// All routes are wrapped by sessionGate (AC-9).
func Register(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := &handler{pool: pool}
	h.initTemplates()

	mux.Handle("/viewer/", sessionGate(http.HandlerFunc(h.handleIndex)))
	mux.Handle("/viewer/api/search", sessionGate(http.HandlerFunc(h.handleSearchAPI)))
	mux.Handle("/viewer/api/projects", sessionGate(http.HandlerFunc(h.handleProjectsAPI)))
	mux.Handle("/viewer/api/node-types", sessionGate(http.HandlerFunc(h.handleNodeTypesAPI)))
}

// sessionGate wraps next with a ch_session cookie check. Discrimination is
// purely path-based (no Accept-header sniffing — fetch() defaults to */*):
//   - /viewer/api/* paths → 401 application/json {"error":"authentication required"}
//   - all other /viewer/* paths → 302 /auth/login?next=<url-encoded current path>
//
// When the session is valid the request passes through to next unchanged.
func sessionGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := web.Read(r); err == nil {
			next.ServeHTTP(w, r)
			return
		}

		if strings.HasPrefix(r.URL.Path, "/viewer/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"authentication required"}`))
			return
		}

		// HTML branch: preserve the original path (including query string) in
		// the ?next= param so the user lands back here after logging in.
		next := r.URL.Path
		if r.URL.RawQuery != "" {
			next += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(next), http.StatusFound)
	})
}

// handler holds shared state for the viewer routes.
type handler struct {
	pool *pgxpool.Pool
	tmpl *template.Template
}

func (h *handler) initTemplates() {
	h.tmpl = template.Must(template.New("index").ParseFS(templateFS, "templates/index.html"))
}

// ── /viewer/ ──────────────────────────────────────────────────────────────────

// handleIndex serves the HTML page. It performs an initial fetch of all nodes
// so the page renders useful content on first load without a client-side JS call.
func (h *handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.ExecuteTemplate(w, "index.html", nil); err != nil {
		slog.Error("viewer: template render failed", "error", err)
	}
}

// ── /viewer/api/search ────────────────────────────────────────────────────────

// searchResponse is the JSON shape returned by /viewer/api/search.
type searchResponse struct {
	Query     string      `json:"query"`
	NodeCount int         `json:"node_count"`
	Nodes     []nodeView  `json:"nodes"`
}

// nodeView is the per-node JSON shape served to the browser.
type nodeView struct {
	Name         string         `json:"name"`
	NodeType     string         `json:"node_type"`
	ProjectID    string         `json:"project_id"`
	Observations []string       `json:"observations"`
	Relations    []relationView `json:"relations"`
	Score        *float64       `json:"score,omitempty"` // 0.0–1.0 similarity; nil on list-all
}

// nodeRowScored pairs a NodeRow with its cosine-distance score from a search query.
// Used only on the searchByCosine path — listAllDesc does not produce scores.
type nodeRowScored struct {
	store.NodeRow
	MinDistance float64
}

// relationView is a single directed edge with both endpoint names resolved.
type relationView struct {
	FromName     string `json:"from_name"`
	ToName       string `json:"to_name"`
	RelationType string `json:"relation_type"`
}

// searchLimit caps results when the operator provides a query. Matches the
// MCP search_nodes tool's LIMIT 10 (store.SearchByCosine) so the viewer
// shows the operator exactly what an agent sees when it calls search_nodes
// with the same query.
const searchLimit = 10

// listAllLimit caps results when no query is provided. Higher than searchLimit
// because the operator is browsing the graph, not mimicking an agent search.
const listAllLimit = 50

// handleSearchAPI handles GET /viewer/api/search?q=...&project=...&nodeType=...
// Empty or missing q → list all active nodes ordered by created_at DESC.
// Non-empty q → semantic search via pgvector cosine, limit 10 (mirrors MCP search_nodes).
// Optional project= filters by project. Optional nodeType= filters by node type.
func (h *handler) handleSearchAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	query    := strings.TrimSpace(r.URL.Query().Get("q"))
	project  := r.URL.Query().Get("project")
	nodeType := r.URL.Query().Get("nodeType")
	ctx      := r.Context()

	var views []nodeView
	var err error

	if query == "" {
		var nodeRows []store.NodeRow
		nodeRows, err = listAllDesc(ctx, h.pool, project, nodeType)
		if err == nil {
			views, err = buildNodeViews(ctx, h.pool, nodeRows, nil)
		}
	} else {
		var scored []nodeRowScored
		scored, err = searchByCosine(ctx, h.pool, query, project, nodeType)
		if err == nil {
			nodeRows := make([]store.NodeRow, len(scored))
			scores := make(map[string]float64, len(scored))
			for i, s := range scored {
				nodeRows[i] = s.NodeRow
				similarity := 1.0 - s.MinDistance
				if similarity < 0 {
					similarity = 0
				}
				if similarity > 1 {
					similarity = 1
				}
				scores[s.ID] = similarity
			}
			views, err = buildNodeViews(ctx, h.pool, nodeRows, scores)
		}
	}

	if err != nil {
		// Distinguish embedder errors (503) from generic DB errors (500).
		if isEmbedderError(err) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "embedder unavailable",
			})
			return
		}
		slog.Error("viewer: search query failed", "query", query, "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, searchResponse{
		Query:     query,
		NodeCount: len(views),
		Nodes:     views,
	})
}

// ── /viewer/api/projects ──────────────────────────────────────────────────────

// projectsResponse is the JSON shape returned by /viewer/api/projects.
type projectsResponse struct {
	Projects []string `json:"projects"`
}

// handleProjectsAPI handles GET /viewer/api/projects. Returns the distinct
// project IDs from active nodes, ordered with "global" first, then the rest
// alphabetically. If "global" has no active nodes it is still always included
// (as the default project it is always conceptually present).
func (h *handler) handleProjectsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	projects, err := fetchDistinctProjects(r.Context(), h.pool)
	if err != nil {
		slog.Error("viewer: fetch projects failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "internal server error",
		})
		return
	}

	writeJSON(w, http.StatusOK, projectsResponse{Projects: projects})
}

// fetchDistinctProjects returns the sorted project list. "global" is always
// first; remaining projects are sorted alphabetically.
func fetchDistinctProjects(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT project_id FROM nodes WHERE deleted_at IS NULL ORDER BY project_id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	seen := map[string]bool{}
	var others []string
	hasGlobal := false

	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return nil, err
		}
		if pid == "global" {
			hasGlobal = true
			continue
		}
		if !seen[pid] {
			seen[pid] = true
			others = append(others, pid)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Strings(others)

	// "global" always appears first, even if there are no active nodes in it
	// (the default project is always conceptually present).
	result := make([]string, 0, 1+len(others))
	_ = hasGlobal // always include global regardless
	result = append(result, "global")
	result = append(result, others...)
	return result, nil
}

// ── /viewer/api/node-types ────────────────────────────────────────────────────

// nodeTypesResponse is the JSON shape returned by /viewer/api/node-types.
type nodeTypesResponse struct {
	NodeTypes []string `json:"nodeTypes"`
}

// handleNodeTypesAPI handles GET /viewer/api/node-types. Returns the static
// sorted list of valid node_type values. No DB access required — the list is
// derived from the validate package's taxonomy table.
func (h *handler) handleNodeTypesAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, http.StatusOK, nodeTypesResponse{NodeTypes: validate.SortedNodeTypes()})
}

// ── DB helpers ────────────────────────────────────────────────────────────────

// listAllDesc returns up to listAllLimit active nodes ordered by creation time
// (newest first). Unlike store.ListActive which orders by name, this gives the
// viewer a "most recently added" default view. Optional project and nodeType
// params filter the result set; empty string means no filter.
func listAllDesc(ctx context.Context, pool *pgxpool.Pool, project, nodeType string) ([]store.NodeRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, name, node_type, project_id
		 FROM nodes
		 WHERE deleted_at IS NULL
		   AND ($2::text = '' OR project_id = $2)
		   AND ($3::text = '' OR node_type  = $3)
		 ORDER BY created_at DESC
		 LIMIT $1`,
		listAllLimit, project, nodeType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []store.NodeRow
	for rows.Next() {
		var r store.NodeRow
		if err := rows.Scan(&r.ID, &r.Name, &r.NodeType, &r.ProjectID); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// searchByCosine embeds query and returns up to searchLimit (10) active nodes
// ranked by minimum cosine distance of their observation embeddings, along with
// that distance. The LIMIT matches store.SearchByCosine used by the MCP
// search_nodes tool. Optional project and nodeType params filter results; empty
// string means no filter.
func searchByCosine(ctx context.Context, pool *pgxpool.Pool, query, project, nodeType string) ([]nodeRowScored, error) {
	vecs, err := embedpkg.Default().Encode(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	queryVec := pgvector.NewVector(vecs[0])

	rows, err := pool.Query(ctx,
		`SELECT n.id, n.name, n.node_type, n.project_id,
		        MIN(o.embedding <=> $1::vector) AS min_distance
		 FROM nodes n
		 JOIN observations o ON o.node_id = n.id
		 WHERE n.deleted_at IS NULL
		   AND o.deleted_at IS NULL
		   AND o.embedding IS NOT NULL
		   AND ($3::text = '' OR n.project_id = $3)
		   AND ($4::text = '' OR n.node_type  = $4)
		 GROUP BY n.id, n.name, n.node_type, n.project_id
		 ORDER BY min_distance ASC
		 LIMIT $2`,
		queryVec,
		searchLimit,
		project,
		nodeType,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []nodeRowScored
	for rows.Next() {
		var s nodeRowScored
		if err := rows.Scan(&s.ID, &s.Name, &s.NodeType, &s.ProjectID, &s.MinDistance); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

// buildNodeViews enriches node rows with their observations and relations,
// transforming them into the viewer-specific JSON shape. Relations are fetched
// in a single batch query (no N+1) for all nodes that touch the result set.
// scores maps node ID → similarity (0.0–1.0); pass nil on the list-all path
// to omit the Score field from every returned nodeView.
func buildNodeViews(ctx context.Context, pool *pgxpool.Pool, nodeRows []store.NodeRow, scores map[string]float64) ([]nodeView, error) {
	if len(nodeRows) == 0 {
		return []nodeView{}, nil
	}

	ids := make([]string, len(nodeRows))
	for i, r := range nodeRows {
		ids[i] = r.ID
	}

	obsMap, err := store.ListByNodeIDs(ctx, pool, ids)
	if err != nil {
		return nil, err
	}

	// Single batch query: all active relations where at least one endpoint is
	// in the result set. Avoids fetching the entire relations table when the
	// graph is large while still showing cross-result-set edges (e.g. a node
	// in the result that supersedes a node not in the result).
	relRows, err := store.RelationsForNodeIDs(ctx, pool, ids)
	if err != nil {
		return nil, err
	}

	relViews := toRelationViews(relRows)

	views := make([]nodeView, len(nodeRows))
	for i, r := range nodeRows {
		obs := obsMap[r.ID]
		if obs == nil {
			obs = []string{}
		}

		v := nodeView{
			Name:         r.Name,
			NodeType:     r.NodeType,
			ProjectID:    r.ProjectID,
			Observations: obs,
			Relations:    relationsForNode(relViews, r.Name),
		}
		if scores != nil {
			sim := scores[r.ID]
			v.Score = &sim
		}
		views[i] = v
	}

	return views, nil
}

// toRelationViews converts store rows to the viewer wire shape.
func toRelationViews(rows []store.RelationRow) []relationView {
	views := make([]relationView, len(rows))
	for i, r := range rows {
		views[i] = relationView{
			FromName:     r.From,
			ToName:       r.To,
			RelationType: r.RelationType,
		}
	}
	return views
}

// relationsForNode filters the pre-fetched relation slice to only those that
// touch the given node name (either as source or target).
func relationsForNode(all []relationView, nodeName string) []relationView {
	var result []relationView
	for _, rv := range all {
		if rv.FromName == nodeName || rv.ToName == nodeName {
			result = append(result, rv)
		}
	}
	if result == nil {
		return []relationView{}
	}
	return result
}

// ── response helpers ──────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("viewer: json encode failed", "error", err)
	}
}

// isEmbedderError returns true when err originates from the embed package
// (ONNX unavailable, model init failure). This signals a 503 to the client.
// The embed package prefixes its errors with "embed:" — that prefix is the
// reliable signal; we do not inspect arbitrary ONNX runtime strings.
func isEmbedderError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "embed:")
}
