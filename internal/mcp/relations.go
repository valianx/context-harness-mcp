package mcp

import (
	"context"
	"fmt"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// RegisterRelations registers the create_relations tool on the server.
// limiter enforces per-IP write-tool rate limits on create_relations.
//
// delete_relations is not exposed — the endpoint is unauthenticated and
// destructive ops on a public surface are a security risk. Use Supabase Studio
// or store.MarkRelationDeleted for operator cleanup.
func RegisterRelations(s *server.MCPServer, pool *pgxpool.Pool, limiter *ratelimit.Limiter) {
	s.AddTool(
		mcplib.NewTool("create_relations",
			mcplib.WithDescription("Create directed relations between nodes. Each relation needs from, to, and relationType. Cross-project relations are rejected."),
			mcplib.WithArray("relations", mcplib.Required()),
		),
		instrumentTool("create_relations", createRelationsHandler(pool, limiter)),
	)
}

// ── create_relations ──────────────────────────────────────────────────────────

type relationInput struct {
	From         string `json:"from"`
	To           string `json:"to"`
	RelationType string `json:"relationType"`
	Project      string `json:"project,omitempty"`
}

type createRelationsArgs struct {
	Relations []relationInput `json:"relations"`
}

func createRelationsHandler(pool *pgxpool.Pool, limiter *ratelimit.Limiter) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if result := checkRateLimit(ctx, limiter); result != nil {
			return result, nil
		}

		var args createRelationsArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		// Validate project naming before the Content Filter when present.
		for _, r := range args.Relations {
			if r.Project != "" {
				if verr := validate.Check(r.Project); verr != nil {
					return verr.ToMCPResult(), nil
				}
			}
		}

		// Build validate.Payload and run the Content Filter before opening any Tx.
		vp := validate.Payload{Relations: make([]validate.Relation, len(args.Relations))}
		for i, r := range args.Relations {
			vp.Relations[i] = validate.Relation{From: r.From, To: r.To, RelationType: r.RelationType}
		}
		if verr := validate.Run(&vp, validate.KindRelations); verr != nil {
			return verr.ToMCPResult(), nil
		}

		userID, email := attributionFromContext(ctx)
		created, err := execCreateRelations(ctx, pool, args.Relations, userID, email)
		if err != nil {
			if isNodeNotFound(err) {
				return errorResult(err.Error()), nil
			}
			if isCrossProjectError(err) {
				verr := &validate.Error{
					Code:    validate.CodeCrossProjectRelation,
					Message: err.Error(),
					Layer:   validate.LayerProject,
				}
				return verr.ToMCPResult(), nil
			}
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{"created": created}), nil
	}
}

func execCreateRelations(ctx context.Context, pool *pgxpool.Pool, relations []relationInput, userID, email *string) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	created := 0
	for _, rel := range relations {
		fromID, _, fromProject, fromFound, err := store.FindByName(ctx, tx, rel.From, nil)
		if err != nil {
			return 0, fmt.Errorf("find node %q: %w", rel.From, err)
		}
		if !fromFound {
			return 0, &nodeNotFoundError{name: rel.From}
		}

		toID, _, toProject, toFound, err := store.FindByName(ctx, tx, rel.To, nil)
		if err != nil {
			return 0, fmt.Errorf("find node %q: %w", rel.To, err)
		}
		if !toFound {
			return 0, &nodeNotFoundError{name: rel.To}
		}

		// Enforce same-project invariant: from and to must share a project.
		if fromProject != toProject {
			return 0, &crossProjectError{
				from:        rel.From,
				to:          rel.To,
				fromProject: fromProject,
				toProject:   toProject,
			}
		}

		// If the caller explicitly provided a project, it must match the nodes' project.
		if rel.Project != "" && rel.Project != fromProject {
			return 0, &crossProjectError{
				from:        rel.From,
				to:          rel.To,
				fromProject: fromProject,
				toProject:   rel.Project,
			}
		}

		_, inserted, err := store.InsertRelation(ctx, tx, fromID, toID, rel.RelationType, fromProject, userID, email)
		if err != nil {
			return 0, fmt.Errorf("insert relation %q→%q: %w", rel.From, rel.To, err)
		}
		if inserted {
			created++
		}
	}

	return created, tx.Commit(ctx)
}

// crossProjectError is returned when a relation is attempted between nodes in
// different projects. It is surfaced as policy/cross-project-relation.
type crossProjectError struct {
	from        string
	to          string
	fromProject string
	toProject   string
}

func (e *crossProjectError) Error() string {
	return fmt.Sprintf(
		"Cross-project relation rejected: nodo %q está en project %q, nodo %q está en project %q. Las relaciones entre projects distintos no están permitidas.",
		e.from, e.fromProject, e.to, e.toProject,
	)
}

// isCrossProjectError returns true when err is a *crossProjectError.
func isCrossProjectError(err error) bool {
	_, ok := err.(*crossProjectError)
	return ok
}

// isNodeNotFound returns true when err is a *nodeNotFoundError.
func isNodeNotFound(err error) bool {
	_, ok := err.(*nodeNotFoundError)
	return ok
}
