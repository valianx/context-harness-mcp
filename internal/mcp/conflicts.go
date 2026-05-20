package mcp

import (
	"context"
	"fmt"
	"log/slog"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

const (
	findConflictsDefaultTopK     = 5
	findConflictsMaxTopK         = 50
	findConflictsDefaultMinSim   = 0.85
	markSupersededMaxReasonBytes = 500
)

// RegisterConflicts registers the find_conflicts and mark_superseded tools.
// limiter enforces per-subject write-tool rate limits on mark_superseded.
// find_conflicts is read-only and does not consume from the rate limiter.
func RegisterConflicts(s *server.MCPServer, pool *pgxpool.Pool, limiter *ratelimit.Limiter) {
	s.AddTool(
		mcplib.NewTool("find_conflicts",
			mcplib.WithDescription("Find nodes in the same project whose observations are semantically similar to the target node. Returns candidates ordered by similarity descending. Read-only — no rate-limit."),
			mcplib.WithString("nodeName", mcplib.Required(), mcplib.Description("Name of the target node to compare against.")),
			mcplib.WithNumber("top_k", mcplib.Description("Maximum number of candidates to return. Default 5, max 50.")),
			mcplib.WithNumber("min_similarity", mcplib.Description("Minimum cosine similarity threshold in [0, 1]. Default 0.85.")),
			mcplib.WithString("project", mcplib.Description("Optional project filter. When set, scopes the search to that project.")),
		),
		instrumentTool("find_conflicts", findConflictsHandler(pool)),
	)

	s.AddTool(
		mcplib.NewTool("mark_superseded",
			mcplib.WithDescription("Create a supersedes relation from new to old (new supersedes old). Optionally soft-delete the old node's observations to hide stale content. Write tool — rate-limited."),
			mcplib.WithString("old", mcplib.Required(), mcplib.Description("Name of the node being superseded (the older version).")),
			mcplib.WithString("new", mcplib.Required(), mcplib.Description("Name of the node that supersedes the old one (the newer version).")),
			mcplib.WithString("reason", mcplib.Description("Optional free-text reason (≤500 chars). Logged via slog — NOT persisted in DB.")),
			mcplib.WithBoolean("archive_old_observations", mcplib.Description("When true, soft-delete all active observations of the old node (sets deleted_at). Default false.")),
			mcplib.WithString("project", mcplib.Description("Optional project scoping hint. Must match the project of both nodes if provided.")),
		),
		instrumentTool("mark_superseded", markSupersededHandler(pool, limiter)),
	)
}

// ── find_conflicts ────────────────────────────────────────────────────────────

type findConflictsArgs struct {
	NodeName      string  `json:"nodeName"`
	TopK          int     `json:"top_k"`
	MinSimilarity float64 `json:"min_similarity"`
	Project       string  `json:"project,omitempty"`
}

func findConflictsHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		var args findConflictsArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		topK, err := clampAndValidateFindConflictsArgs(&args)
		if err != nil {
			return err, nil
		}

		return execFindConflicts(ctx, pool, args, topK)
	}
}

// clampAndValidateFindConflictsArgs applies defaults, clamps top_k, and
// validates min_similarity. Returns (topK, nil) on success or (nil, errorResult)
// on validation failure.
func clampAndValidateFindConflictsArgs(args *findConflictsArgs) (int, *mcplib.CallToolResult) {
	// Apply defaults.
	if args.TopK <= 0 {
		args.TopK = findConflictsDefaultTopK
	}
	if args.MinSimilarity == 0 {
		args.MinSimilarity = findConflictsDefaultMinSim
	}

	// Clamp top_k to [1, 50].
	topK := args.TopK
	if topK < 1 {
		topK = 1
	}
	if topK > findConflictsMaxTopK {
		topK = findConflictsMaxTopK
	}

	// Validate min_similarity strictly in [0, 1].
	if args.MinSimilarity < 0 || args.MinSimilarity > 1 {
		return 0, errorResult("min_similarity must be in [0, 1]")
	}

	return topK, nil
}

func execFindConflicts(ctx context.Context, pool *pgxpool.Pool, args findConflictsArgs, topK int) (*mcplib.CallToolResult, error) {
	// Resolve the target node. Use nil project filter when project is absent so
	// the store returns the first homonym by project_id ASC (stable tiebreak).
	projectFilter := projectFilterFrom(args.Project)

	// FindByName requires a pgx.Tx. We open a read-only transaction for the
	// lookup and roll it back immediately — no writes, no commit needed.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errorResult(fmt.Sprintf("db error: %s", err)), nil
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	targetID, _, targetProject, found, err := store.FindByName(ctx, tx, args.NodeName, projectFilter)
	if err != nil {
		return errorResult(fmt.Sprintf("db error: %s", err)), nil
	}
	if !found {
		verr := &validate.Error{
			Code:    validate.CodeNodeNotFound,
			Message: fmt.Sprintf("Node not found: %s", args.NodeName),
			Layer:   validate.LayerProject,
		}
		return verr.ToMCPResult(), nil
	}

	// Release the Tx before the pool-based similarity queries.
	_ = tx.Rollback(ctx)

	candidates, err := store.FindSimilarTo(ctx, pool, targetID, targetProject, topK, float32(args.MinSimilarity))
	if err != nil {
		return errorResult(fmt.Sprintf("db error: %s", err)), nil
	}

	return jsonResult(buildFindConflictsResponse(candidates)), nil
}

// candidateJSON is the wire shape for a single find_conflicts candidate.
type candidateJSON struct {
	Name                    string              `json:"name"`
	NodeType                string              `json:"node_type"`
	Similarity              float32             `json:"similarity"`
	MatchingObservationPair observationPairJSON `json:"matching_observations_pair"`
}

// observationPairJSON holds the target and candidate observation texts that
// produced the highest similarity score for this candidate.
type observationPairJSON struct {
	OwnObs   string `json:"own_obs"`
	OtherObs string `json:"other_obs"`
}

func buildFindConflictsResponse(candidates []store.ConflictCandidate) map[string]any {
	out := make([]candidateJSON, len(candidates))
	for i, c := range candidates {
		out[i] = candidateJSON{
			Name:     c.Name,
			NodeType: c.NodeType,
			Similarity: c.Similarity,
			MatchingObservationPair: observationPairJSON{
				OwnObs:   c.OwnObs,
				OtherObs: c.OtherObs,
			},
		}
	}
	// make always returns a non-nil slice; JSON marshals as [] not null.
	return map[string]any{"candidates": out}
}

// ── mark_superseded ───────────────────────────────────────────────────────────

type markSupersededArgs struct {
	Old                    string `json:"old"`
	New                    string `json:"new"`
	Reason                 string `json:"reason,omitempty"`
	ArchiveOldObservations bool   `json:"archive_old_observations"`
	Project                string `json:"project,omitempty"`
}

func markSupersededHandler(pool *pgxpool.Pool, limiter *ratelimit.Limiter) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if result := checkRateLimit(ctx, limiter); result != nil {
			return result, nil
		}

		var args markSupersededArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		if result := validateMarkSupersededArgs(args); result != nil {
			return result, nil
		}

		userID, email := attributionFromContext(ctx)
		return execMarkSuperseded(ctx, pool, args, userID, email)
	}
}

// validateMarkSupersededArgs performs pre-DB input validation.
func validateMarkSupersededArgs(args markSupersededArgs) *mcplib.CallToolResult {
	if args.Old == "" {
		return errorResult("old is required")
	}
	if args.New == "" {
		return errorResult("new is required")
	}
	if args.Old == args.New {
		return errorResult("old and new cannot reference the same node")
	}
	if len(args.Reason) > markSupersededMaxReasonBytes {
		return errorResult(fmt.Sprintf("reason must be ≤%d characters", markSupersededMaxReasonBytes))
	}
	return nil
}

func execMarkSuperseded(
	ctx context.Context,
	pool *pgxpool.Pool,
	args markSupersededArgs,
	userID, email *string,
) (*mcplib.CallToolResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errorResult(fmt.Sprintf("db error: %s", err)), nil
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Resolve both nodes. nil projectFilter: stable tiebreak by project_id ASC.
	oldID, _, oldProject, oldFound, err := store.FindByName(ctx, tx, args.Old, nil)
	if err != nil {
		return errorResult(fmt.Sprintf("db error: %s", err)), nil
	}
	if !oldFound {
		verr := &validate.Error{
			Code:    validate.CodeNodeNotFound,
			Message: fmt.Sprintf("Node not found: %s", args.Old),
			Layer:   validate.LayerProject,
		}
		return verr.ToMCPResult(), nil
	}

	newID, _, newProject, newFound, err := store.FindByName(ctx, tx, args.New, nil)
	if err != nil {
		return errorResult(fmt.Sprintf("db error: %s", err)), nil
	}
	if !newFound {
		verr := &validate.Error{
			Code:    validate.CodeNodeNotFound,
			Message: fmt.Sprintf("Node not found: %s", args.New),
			Layer:   validate.LayerProject,
		}
		return verr.ToMCPResult(), nil
	}

	// Enforce same-project invariant.
	if oldProject != newProject {
		verr := &validate.Error{
			Code:    validate.CodeCrossProjectRelation,
			Message: fmt.Sprintf("Cross-project relation rejected: node %q is in project %q, node %q is in project %q.", args.Old, oldProject, args.New, newProject),
			Layer:   validate.LayerProject,
		}
		return verr.ToMCPResult(), nil
	}

	// If the caller provided an explicit project hint, it must match the nodes' project.
	if args.Project != "" && args.Project != oldProject {
		verr := &validate.Error{
			Code:    validate.CodeCrossProjectRelation,
			Message: fmt.Sprintf("Cross-project relation rejected: explicit project %q does not match nodes' project %q.", args.Project, oldProject),
			Layer:   validate.LayerProject,
		}
		return verr.ToMCPResult(), nil
	}

	projectID := oldProject

	// Insert the supersedes relation: direction is new → old (new supersedes old).
	_, inserted, err := store.InsertRelation(ctx, tx, newID, oldID, "supersedes", projectID, userID, email)
	if err != nil {
		return errorResult(fmt.Sprintf("db error: %s", err)), nil
	}
	if !inserted {
		verr := &validate.Error{
			Code:    validate.CodeRelationAlreadyExists,
			Message: fmt.Sprintf("Relation supersedes(%s → %s) already exists.", args.New, args.Old),
			Layer:   validate.LayerProject,
		}
		return verr.ToMCPResult(), nil
	}

	// Optionally archive the old node's observations.
	archivedCount := 0
	if args.ArchiveOldObservations {
		count, err := archiveObservations(ctx, tx, oldID)
		if err != nil {
			return errorResult(fmt.Sprintf("db error archiving observations: %s", err)), nil
		}
		archivedCount = count
	}

	if err := tx.Commit(ctx); err != nil {
		return errorResult(fmt.Sprintf("db error: %s", err)), nil
	}

	// Structured audit log. reason is logged but NOT persisted in DB.
	userIDStr := ""
	if userID != nil {
		userIDStr = *userID
	}
	emailStr := ""
	if email != nil {
		emailStr = *email
	}
	slog.Info("mark_superseded",
		"old", args.Old,
		"new", args.New,
		"old_node_id", oldID,
		"new_node_id", newID,
		"project_id", projectID,
		"reason", args.Reason,
		"archive_observations", args.ArchiveOldObservations,
		"observations_archived", archivedCount,
		"user_id", userIDStr,
		"email", emailStr,
	)

	return jsonResult(map[string]any{
		"relation_created":      true,
		"observations_archived": archivedCount,
	}), nil
}

// archiveObservations soft-deletes all active observations for the given node.
// Returns the number of rows updated.
func archiveObservations(ctx context.Context, tx pgx.Tx, nodeID string) (int, error) {
	tag, err := tx.Exec(ctx,
		`UPDATE observations SET deleted_at = now()
		 WHERE node_id = $1 AND deleted_at IS NULL`,
		nodeID,
	)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
