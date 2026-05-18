package mcp

import (
	"context"
	"fmt"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

// RegisterQuery registers the search_nodes, open_nodes, and read_graph tools.
func RegisterQuery(s *server.MCPServer, pool *pgxpool.Pool) {
	s.AddTool(
		mcplib.NewTool("search_nodes",
			mcplib.WithDescription("Search entities by substring match on observation text. Returns matching entities and the relations between them."),
			mcplib.WithString("query", mcplib.Required()),
		),
		searchNodesHandler(pool),
	)

	s.AddTool(
		mcplib.NewTool("open_nodes",
			mcplib.WithDescription("Retrieve specific entities by name, plus the relations between them."),
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

// ── wire-shape types (match Python reference shapes byte-for-byte) ───────────

// entityJSON mirrors the Python _get_entity return shape:
// {"name":..., "entityType":..., "observations":[...]}
type entityJSON struct {
	Name         string   `json:"name"`
	EntityType   string   `json:"entityType"`
	Observations []string `json:"observations"`
}

// relationJSON mirrors the Python relation shape:
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

		entities, relations, err := searchNodes(ctx, pool, args.Query)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{
			"entities":  entities,
			"relations": relations,
		}), nil
	}
}

func searchNodes(ctx context.Context, pool *pgxpool.Pool, query string) ([]entityJSON, []relationJSON, error) {
	// Encode the query into a vector; fail loudly if the embedder is broken.
	// The substring helper (store.SearchByTextSubstring) stays in the codebase
	// but is no longer wired here — failure must be visible to operators.
	vecs, err := embed.Default().Encode(ctx, []string{query})
	if err != nil {
		return nil, nil, fmt.Errorf("search_nodes: embedding failed: %w", err)
	}
	queryVec := pgvector.NewVector(vecs[0])

	entityRows, err := store.SearchByCosine(ctx, pool, queryVec)
	if err != nil {
		return nil, nil, err
	}

	return buildEntityRelationResult(ctx, pool, entityRows)
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

		entities, relations, err := openNodes(ctx, pool, args.Names)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{
			"entities":  entities,
			"relations": relations,
		}), nil
	}
}

func openNodes(ctx context.Context, pool *pgxpool.Pool, names []string) ([]entityJSON, []relationJSON, error) {
	if len(names) == 0 {
		return []entityJSON{}, []relationJSON{}, nil
	}

	// Resolve each name to an active entity row via a single pool query.
	rows, err := pool.Query(ctx,
		`SELECT id, name, entity_type
		 FROM entities
		 WHERE name = ANY($1) AND deleted_at IS NULL
		 ORDER BY name`,
		names,
	)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var entityRows []store.EntityRow
	for rows.Next() {
		var r store.EntityRow
		if err := rows.Scan(&r.ID, &r.Name, &r.EntityType); err != nil {
			return nil, nil, err
		}
		entityRows = append(entityRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	return buildEntityRelationResult(ctx, pool, entityRows)
}

// ── read_graph ───────────────────────────────────────────────────────────────

func readGraphHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		entityRows, err := store.ListActive(ctx, pool)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		entities, relations, err := buildEntityRelationResult(ctx, pool, entityRows)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{
			"entities":       entities,
			"relations":      relations,
			"entity_count":   len(entities),
			"relation_count": len(relations),
		}), nil
	}
}

// ── shared builder ───────────────────────────────────────────────────────────

// buildEntityRelationResult converts a slice of entity rows into the wire-shape
// slices expected by all three query tools. It fetches observations and
// relations in two additional pool queries.
func buildEntityRelationResult(ctx context.Context, pool *pgxpool.Pool, entityRows []store.EntityRow) ([]entityJSON, []relationJSON, error) {
	if len(entityRows) == 0 {
		return []entityJSON{}, []relationJSON{}, nil
	}

	ids := make([]string, len(entityRows))
	for i, r := range entityRows {
		ids[i] = r.ID
	}

	obsMap, err := store.ListByEntityIDs(ctx, pool, ids)
	if err != nil {
		return nil, nil, err
	}

	relationRows, err := store.ListActiveRelationsForEntityIDs(ctx, pool, ids)
	if err != nil {
		return nil, nil, err
	}

	entities := make([]entityJSON, len(entityRows))
	for i, r := range entityRows {
		obs := obsMap[r.ID]
		if obs == nil {
			obs = []string{}
		}
		entities[i] = entityJSON{
			Name:         r.Name,
			EntityType:   r.EntityType,
			Observations: obs,
		}
	}

	relations := make([]relationJSON, len(relationRows))
	for i, r := range relationRows {
		relations[i] = relationJSON{From: r.From, To: r.To, RelationType: r.RelationType}
	}

	return entities, relations, nil
}
