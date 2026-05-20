// Package viewer serves a public single-page web UI for browsing the knowledge
// graph. It registers three routes on the caller's mux:
//
//   - GET /viewer/          — serves the embedded HTML page
//   - GET /viewer/api/search — returns JSON (node list or semantic search results)
//   - GET /viewer/api/projects — returns the list of distinct project IDs
//
// Access is intentionally unauthenticated — same exposure level as the MCP read
// tools (search_nodes, read_graph). User chose Option A (public, no auth) in the
// feature brief.
package viewer

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"

	embedpkg "github.com/mariogutierrez/context-harness-mcp/internal/embed"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

//go:embed templates/*
var templateFS embed.FS

// Register adds the /viewer/, /viewer/api/search, and /viewer/api/projects
// routes to mux. pool must be non-nil — both handlers require DB access.
func Register(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := &handler{pool: pool}
	h.initTemplates()

	mux.HandleFunc("/viewer/", h.handleIndex)
	mux.HandleFunc("/viewer/api/search", h.handleSearchAPI)
	mux.HandleFunc("/viewer/api/projects", h.handleProjectsAPI)
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
	Name         string        `json:"name"`
	NodeType     string        `json:"node_type"`
	ProjectID    string        `json:"project_id"`
	Observations []string      `json:"observations"`
	RelationsOut []relationOut `json:"relations_out"`
	RelationsIn  []relationIn  `json:"relations_in"`
}

type relationOut struct {
	To   string `json:"to"`
	Type string `json:"type"`
}

type relationIn struct {
	From string `json:"from"`
	Type string `json:"type"`
}

const searchLimit = 50

// handleSearchAPI handles GET /viewer/api/search?q=...&project=...
// Empty or missing q → list all active nodes ordered by created_at DESC.
// Non-empty q → semantic search via pgvector cosine, limit 50.
// Optional project= filters by project.
func (h *handler) handleSearchAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	project := r.URL.Query().Get("project")
	ctx := r.Context()

	var nodeRows []store.NodeRow
	var err error

	if query == "" {
		nodeRows, err = listAllDesc(ctx, h.pool, project)
	} else {
		nodeRows, err = searchByCosine(ctx, h.pool, query, project)
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

	views, err := buildNodeViews(ctx, h.pool, nodeRows)
	if err != nil {
		slog.Error("viewer: building node views failed", "error", err)
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

// ── DB helpers ────────────────────────────────────────────────────────────────

// listAllDesc returns up to searchLimit active nodes ordered by creation time
// (newest first). Unlike store.ListActive which orders by name, this gives the
// viewer a "most recently added" default view. Optional project filters by project.
func listAllDesc(ctx context.Context, pool *pgxpool.Pool, project string) ([]store.NodeRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, name, node_type, project_id
		 FROM nodes
		 WHERE deleted_at IS NULL
		   AND ($2::text = '' OR project_id = $2)
		 ORDER BY created_at DESC
		 LIMIT $1`,
		searchLimit, project,
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

// searchByCosine embeds query and returns up to searchLimit active nodes ranked
// by minimum cosine distance of their observation embeddings. Optional project
// filters results to a specific project.
func searchByCosine(ctx context.Context, pool *pgxpool.Pool, query, project string) ([]store.NodeRow, error) {
	vecs, err := embedpkg.Default().Encode(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	queryVec := pgvector.NewVector(vecs[0])

	rows, err := pool.Query(ctx,
		`SELECT n.id, n.name, n.node_type, n.project_id
		 FROM nodes n
		 JOIN observations o ON o.node_id = n.id
		 WHERE n.deleted_at IS NULL
		   AND o.deleted_at IS NULL
		   AND o.embedding IS NOT NULL
		   AND ($3::text = '' OR n.project_id = $3)
		 GROUP BY n.id, n.name, n.node_type, n.project_id
		 ORDER BY MIN(o.embedding <=> $1::vector) ASC
		 LIMIT $2`,
		queryVec,
		searchLimit,
		project,
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

// buildNodeViews enriches node rows with their observations and relations,
// transforming them into the viewer-specific JSON shape.
func buildNodeViews(ctx context.Context, pool *pgxpool.Pool, nodeRows []store.NodeRow) ([]nodeView, error) {
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

	allRelations, err := store.ListActiveRelations(ctx, pool)
	if err != nil {
		return nil, err
	}

	views := make([]nodeView, len(nodeRows))
	for i, r := range nodeRows {
		obs := obsMap[r.ID]
		if obs == nil {
			obs = []string{}
		}

		out, in := partitionRelations(allRelations, r.Name)

		views[i] = nodeView{
			Name:         r.Name,
			NodeType:     r.NodeType,
			ProjectID:    r.ProjectID,
			Observations: obs,
			RelationsOut: out,
			RelationsIn:  in,
		}
	}

	return views, nil
}

// partitionRelations splits active relations into outbound and inbound slices
// for the given node name. Relations where neither endpoint matches are skipped.
func partitionRelations(relations []store.RelationRow, nodeName string) ([]relationOut, []relationIn) {
	var out []relationOut
	var in []relationIn

	for _, rel := range relations {
		switch {
		case rel.From == nodeName:
			out = append(out, relationOut{To: rel.To, Type: rel.RelationType})
		case rel.To == nodeName:
			in = append(in, relationIn{From: rel.From, Type: rel.RelationType})
		}
	}

	if out == nil {
		out = []relationOut{}
	}
	if in == nil {
		in = []relationIn{}
	}

	return out, in
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
