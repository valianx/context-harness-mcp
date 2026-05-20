package mcp

import (
	"context"
	"fmt"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

// RegisterQuery registers the search_nodes, open_nodes, read_graph, stats, and timeline tools.
func RegisterQuery(s *server.MCPServer, pool *pgxpool.Pool) {
	s.AddTool(
		mcplib.NewTool("timeline",
			mcplib.WithDescription("List active nodes in reverse-chronological order (newest first). Supports optional RFC3339 date bounds and offset-based pagination. Read-only — no rate-limit."),
			mcplib.WithString("since", mcplib.Description("Lower bound (inclusive) on created_at. RFC3339 format, e.g. 2026-05-01T00:00:00Z. Optional.")),
			mcplib.WithString("until", mcplib.Description("Upper bound (inclusive) on created_at. RFC3339 format. Optional.")),
			mcplib.WithNumber("limit", mcplib.Description("Page size. Default 50, max 200. Values outside [1,200] are silently clamped.")),
			mcplib.WithNumber("offset", mcplib.Description("Row offset for pagination. Default 0, max 100000. Values outside [0,100000] are silently clamped.")),
		),
		timelineHandler(pool),
	)

	s.AddTool(
		mcplib.NewTool("stats",
			mcplib.WithDescription("Return aggregated counts for the active knowledge graph: node_count, observation_count, relation_count, by_type breakdown, and oldest/newest node. Read-only — no arguments required."),
		),
		statsHandler(pool),
	)

	s.AddTool(
		mcplib.NewTool("search_nodes",
			mcplib.WithDescription("Search nodes by semantic similarity of observation text. Returns matching nodes and the relations between them."),
			mcplib.WithString("query", mcplib.Required()),
		),
		searchNodesHandler(pool),
	)

	s.AddTool(
		mcplib.NewTool("open_nodes",
			mcplib.WithDescription("Retrieve specific nodes by name, plus the relations between them."),
			mcplib.WithArray("names", mcplib.Required()),
		),
		openNodesHandler(pool),
	)

	s.AddTool(
		mcplib.NewTool("read_graph",
			mcplib.WithDescription("Read the entire active knowledge graph. Use sparingly — prefer search_nodes for targeted queries."),
		),
		readGraphHandler(pool),
	)
}

// ── wire-shape types ─────────────────────────────────────────────────────────

// nodeJSON is the wire shape for a single node in query results:
// {"name":..., "nodeType":..., "observations":[...]}
type nodeJSON struct {
	Name         string   `json:"name"`
	NodeType     string   `json:"nodeType"`
	Observations []string `json:"observations"`
}

// relationJSON is the wire shape for a single relation:
// {"from":..., "to":..., "relationType":...}
type relationJSON struct {
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
}

// ── search_nodes ─────────────────────────────────────────────────────────────

type searchNodesArgs struct {
	Query string `json:"query"`
}

func searchNodesHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		var args searchNodesArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		nodes, relations, err := searchNodes(ctx, pool, args.Query)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{
			"nodes":     nodes,
			"relations": relations,
		}), nil
	}
}

func searchNodes(ctx context.Context, pool *pgxpool.Pool, query string) ([]nodeJSON, []relationJSON, error) {
	// Encode the query into a vector; fail loudly if the embedder is broken.
	// The substring helper (store.SearchByTextSubstring) stays in the codebase
	// but is no longer wired here — failure must be visible to operators.
	vecs, err := embed.Default().Encode(ctx, []string{query})
	if err != nil {
		return nil, nil, fmt.Errorf("search_nodes: embedding failed: %w", err)
	}
	queryVec := pgvector.NewVector(vecs[0])

	nodeRows, err := store.SearchByCosine(ctx, pool, queryVec)
	if err != nil {
		return nil, nil, err
	}

	return buildNodeRelationResult(ctx, pool, nodeRows)
}

// ── open_nodes ───────────────────────────────────────────────────────────────

type openNodesArgs struct {
	Names []string `json:"names"`
}

func openNodesHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		var args openNodesArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		nodes, relations, err := openNodes(ctx, pool, args.Names)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{
			"nodes":     nodes,
			"relations": relations,
		}), nil
	}
}

func openNodes(ctx context.Context, pool *pgxpool.Pool, names []string) ([]nodeJSON, []relationJSON, error) {
	if len(names) == 0 {
		return []nodeJSON{}, []relationJSON{}, nil
	}

	// Resolve each name to an active node row via a single pool query.
	rows, err := pool.Query(ctx,
		`SELECT id, name, node_type
		 FROM nodes
		 WHERE name = ANY($1) AND deleted_at IS NULL
		 ORDER BY name`,
		names,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var nodeRows []store.NodeRow
	for rows.Next() {
		var r store.NodeRow
		if err := rows.Scan(&r.ID, &r.Name, &r.NodeType); err != nil {
			return nil, nil, err
		}
		nodeRows = append(nodeRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	return buildNodeRelationResult(ctx, pool, nodeRows)
}

// ── stats ─────────────────────────────────────────────────────────────────────

// statsHandler returns aggregated KG counts. Read-only: no rate-limit, no
// content-filter, no arguments. Mirrors the read_graph pattern.
func statsHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		result, err := store.Stats(ctx, pool)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}
		return jsonResult(result), nil
	}
}

// ── read_graph ───────────────────────────────────────────────────────────────

func readGraphHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		nodeRows, err := store.ListActive(ctx, pool)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		nodes, relations, err := buildNodeRelationResult(ctx, pool, nodeRows)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{
			"nodes":          nodes,
			"relations":      relations,
			"node_count":     len(nodes),
			"relation_count": len(relations),
		}), nil
	}
}

// ── timeline ──────────────────────────────────────────────────────────────────

const (
	timelineDefaultLimit  = 50
	timelineMaxLimit      = 200
	timelineMaxOffset     = 100_000
)

type timelineArgs struct {
	Since  string `json:"since"`
	Until  string `json:"until"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

// timelineHandler lists active nodes in reverse-chronological order with
// optional date bounds and offset-based pagination. Read-only: no rate-limit,
// no content filter. Invalid since/until produce a structured error response
// without hitting the DB. limit/offset are silently clamped to valid ranges.
func timelineHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		var args timelineArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		since, until, err := parseTimelineBounds(args.Since, args.Until)
		if err != nil {
			return errorResult(err.Error()), nil
		}

		limit, offset := clampTimelinePagination(args.Limit, args.Offset)

		nodeRows, hasMore, err := store.ListByCreatedAt(ctx, pool, since, until, limit, offset)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		nodes, relations, err := buildNodeRelationResult(ctx, pool, nodeRows)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{
			"nodes":      nodes,
			"relations":  relations,
			"node_count": len(nodes),
			"has_more":   hasMore,
		}), nil
	}
}

// parseTimelineBounds parses the optional since/until RFC3339 strings.
// Returns nil pointers when a field is empty (no bound). Returns an error
// with a user-facing message when a non-empty field fails to parse.
func parseTimelineBounds(since, until string) (*time.Time, *time.Time, error) {
	var sinceT, untilT *time.Time

	if since != "" {
		t, err := time.Parse(time.RFC3339, since)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid since: must be RFC3339")
		}
		sinceT = &t
	}

	if until != "" {
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid until: must be RFC3339")
		}
		untilT = &t
	}

	return sinceT, untilT, nil
}

// clampTimelinePagination returns a limit clamped to [1, 200] and an offset
// clamped to [0, 100000]. Zero/negative limit falls back to the default of 50.
func clampTimelinePagination(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = timelineDefaultLimit
	}
	if limit > timelineMaxLimit {
		limit = timelineMaxLimit
	}
	if offset < 0 {
		offset = 0
	}
	if offset > timelineMaxOffset {
		offset = timelineMaxOffset
	}
	return limit, offset
}

// ── shared builder ────────────────────────────────────────────────────────────

// buildNodeRelationResult converts a slice of node rows into the wire-shape
// slices expected by all three query tools. It fetches observations and
// relations in two additional pool queries.
func buildNodeRelationResult(ctx context.Context, pool *pgxpool.Pool, nodeRows []store.NodeRow) ([]nodeJSON, []relationJSON, error) {
	if len(nodeRows) == 0 {
		return []nodeJSON{}, []relationJSON{}, nil
	}

	ids := make([]string, len(nodeRows))
	for i, r := range nodeRows {
		ids[i] = r.ID
	}

	obsMap, err := store.ListByNodeIDs(ctx, pool, ids)
	if err != nil {
		return nil, nil, err
	}

	relationRows, err := store.ListActiveRelationsForNodeIDs(ctx, pool, ids)
	if err != nil {
		return nil, nil, err
	}

	nodes := make([]nodeJSON, len(nodeRows))
	for i, r := range nodeRows {
		obs := obsMap[r.ID]
		if obs == nil {
			obs = []string{}
		}
		nodes[i] = nodeJSON{
			Name:         r.Name,
			NodeType:     r.NodeType,
			Observations: obs,
		}
	}

	relations := make([]relationJSON, len(relationRows))
	for i, r := range relationRows {
		relations[i] = relationJSON{From: r.From, To: r.To, RelationType: r.RelationType}
	}

	return nodes, relations, nil
}
