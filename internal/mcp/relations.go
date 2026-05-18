package mcp

import (
	"context"
	"fmt"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// RegisterRelations registers the create_relations and delete_relations tools.
func RegisterRelations(s *server.MCPServer, pool *pgxpool.Pool) {
	s.AddTool(
		mcplib.NewTool("create_relations",
			mcplib.WithDescription("Create directed relations between entities. Each relation needs from, to, and relationType."),
			mcplib.WithArray("relations", mcplib.Required()),
		),
		createRelationsHandler(pool),
	)

	s.AddTool(
		mcplib.NewTool("delete_relations",
			mcplib.WithDescription("Soft-delete relations by (from, to, relationType) triple."),
			mcplib.WithArray("relations", mcplib.Required()),
		),
		deleteRelationsHandler(pool),
	)
}

// ── create_relations ─────────────────────────────────────────────────────────

type relationInput struct {
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
}

type createRelationsArgs struct {
	Relations []relationInput `json:"relations"`
}

func createRelationsHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		var args createRelationsArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		// Build validate.Payload and run the Content Filter before opening any Tx.
		vp := validate.Payload{Relations: make([]validate.Relation, len(args.Relations))}
		for i, r := range args.Relations {
			vp.Relations[i] = validate.Relation{From: r.From, To: r.To, RelationType: r.RelationType}
		}
		if verr := validate.Run(vp, validate.KindRelations); verr != nil {
			return verr.ToMCPResult(), nil
		}

		created, err := execCreateRelations(ctx, pool, args.Relations)
		if err != nil {
			if isEntityNotFound(err) {
				return errorResult(err.Error()), nil
			}
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{"created": created}), nil
	}
}

func execCreateRelations(ctx context.Context, pool *pgxpool.Pool, relations []relationInput) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	created := 0
	for _, rel := range relations {
		fromID, _, fromFound, err := store.FindByName(ctx, tx, rel.From)
		if err != nil {
			return 0, fmt.Errorf("find entity %q: %w", rel.From, err)
		}
		if !fromFound {
			return 0, &entityNotFoundError{name: rel.From}
		}

		toID, _, toFound, err := store.FindByName(ctx, tx, rel.To)
		if err != nil {
			return 0, fmt.Errorf("find entity %q: %w", rel.To, err)
		}
		if !toFound {
			return 0, &entityNotFoundError{name: rel.To}
		}

		_, inserted, err := store.InsertRelation(ctx, tx, fromID, toID, rel.RelationType)
		if err != nil {
			return 0, fmt.Errorf("insert relation %q→%q: %w", rel.From, rel.To, err)
		}
		if inserted {
			created++
		}
	}

	return created, tx.Commit(ctx)
}

// ── delete_relations ─────────────────────────────────────────────────────────

type deleteRelationsArgs struct {
	Relations []relationInput `json:"relations"`
}

func deleteRelationsHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		var args deleteRelationsArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		// No Content Filter for delete operations.
		tx, err := pool.Begin(ctx)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}
		defer tx.Rollback(ctx) //nolint:errcheck

		deleted := 0
		for _, rel := range args.Relations {
			n, err := store.MarkRelationDeleted(ctx, tx, rel.From, rel.To, rel.RelationType)
			if err != nil {
				return errorResult(fmt.Sprintf("db error: %s", err)), nil
			}
			deleted += n
		}

		if err := tx.Commit(ctx); err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{"deleted": deleted}), nil
	}
}

// isEntityNotFound returns true when err is an *entityNotFoundError.
func isEntityNotFound(err error) bool {
	_, ok := err.(*entityNotFoundError)
	return ok
}
