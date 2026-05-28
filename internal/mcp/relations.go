package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel/codes"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
	"github.com/mariogutierrez/context-harness-mcp/internal/observability"
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
		start := time.Now()
		ctx, span := toolTracer().Start(ctx, "mcp.create_relations")
		defer span.End()

		span.SetAttributes(attrToolName.String("create_relations"))

		outcome := outcomeServerError
		defer func() {
			span.SetAttributes(attrToolOutcome.String(outcome))
			observability.RecordRequest(ctx, "create_relations", outcome, time.Since(start))
			slog.InfoContext(ctx, "request_completed",
				"tool", "create_relations",
				"outcome", outcome,
				"duration_ms", time.Since(start).Milliseconds(),
				"user.id", auth.UserIDFromContext(ctx),
			)
		}()

		if uid := auth.UserIDFromContext(ctx); uid != "" {
			span.SetAttributes(attrUserID.String(uid))
		}

		if result := checkRateLimit(ctx, limiter); result != nil {
			outcome = outcomePolicyReject
			return result, nil
		}

		var args createRelationsArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		// Emit relations summary after BindArguments — appears on all exit paths (AC-I.5).
		if len(args.Relations) > 0 {
			span.SetAttributes(attrRelationsSummary.StringSlice(relationsSummary(args.Relations, relationsMaxCount)))
		}

		// Validate project naming before the Content Filter when present.
		for _, r := range args.Relations {
			if r.Project != "" {
				if verr := validate.Check(r.Project); verr != nil {
					outcome = outcomePolicyReject
					span.SetAttributes(
						attrErrorCode.String(verr.Code),
						attrLayer.String(verr.Layer),
					)
					setRejectedInputAttr(span, firstRelationFragment(args.Relations))
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
			outcome = outcomePolicyReject
			span.SetStatus(codes.Error, verr.Code)
			span.SetAttributes(
				attrErrorCode.String(verr.Code),
				attrLayer.String(verr.Layer),
			)
			setRejectedInputAttr(span, firstRelationFragment(args.Relations))
			observability.RecordValidationRejection(ctx, verr.Layer, verr.MatchedPattern)
			return verr.ToMCPResult(), nil
		}

		// Count metadata — no content in span attributes (AC-E.3).
		span.SetAttributes(attrNodeCount.Int(len(args.Relations)))

		userID, email := attributionFromContext(ctx)
		created, err := execCreateRelations(ctx, pool, args.Relations, userID, email)
		if err != nil {
			if isNodeNotFound(err) {
				span.SetStatus(codes.Error, err.Error())
				setRejectedInputAttr(span, firstRelationFragment(args.Relations))
				return errorResult(err.Error()), nil
			}
			if isCrossProjectError(err) {
				verr := &validate.Error{
					Code:    validate.CodeCrossProjectRelation,
					Message: err.Error(),
					Layer:   validate.LayerProject,
				}
				outcome = outcomePolicyReject
				span.SetStatus(codes.Error, verr.Code)
				span.SetAttributes(
					attrErrorCode.String(verr.Code),
					attrLayer.String(verr.Layer),
				)
				setRejectedInputAttr(span, firstRelationFragment(args.Relations))
				return verr.ToMCPResult(), nil
			}
			span.SetStatus(codes.Error, err.Error())
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		outcome = outcomeSuccess
		return jsonResult(map[string]any{"created": created}), nil
	}
}

// ── relations.go span-attr helpers ────────────────────────────────────────────

// relationsSummary builds a slice of "from→to:type" strings capped at maxCount (AC-I.5).
func relationsSummary(relations []relationInput, maxCount int) []string {
	n := len(relations)
	if n > maxCount {
		n = maxCount
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		r := relations[i]
		out[i] = fmt.Sprintf("%s→%s:%s", r.From, r.To, r.RelationType)
	}
	return out
}

// firstRelationFragment returns a short JSON-like fragment of the first relation
// for use as mcp.rejected_input. Returns empty string when relations is empty.
func firstRelationFragment(relations []relationInput) string {
	if len(relations) == 0 {
		return ""
	}
	r := relations[0]
	return fmt.Sprintf(`{"from":%q,"to":%q,"relationType":%q}`, r.From, r.To, r.RelationType)
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
