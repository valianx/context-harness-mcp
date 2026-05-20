package mcp

import (
	"context"
	"fmt"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

const sessionSummaryMaxBytes = 4096

// RegisterSessions registers the session_start, session_end, and
// session_summary tools. limiter enforces per-subject write-tool rate limits on
// session_start and session_end. session_summary is read-only and is not
// rate-limited.
func RegisterSessions(s *server.MCPServer, pool *pgxpool.Pool, limiter *ratelimit.Limiter) {
	s.AddTool(
		mcplib.NewTool("session_start",
			mcplib.WithDescription("Start a new session. Returns a session_id to pass to subsequent create_nodes calls. A session is a unit of work that groups nodes by context. Session boundaries are informational — not security boundaries."),
			mcplib.WithString("project", mcplib.Description("Optional project tag for this session. Default 'global'. Must match the project naming regex.")),
			mcplib.WithString("working_dir", mcplib.Description("Optional working directory for this session (e.g. /home/user/zippy).")),
		),
		instrumentTool("session_start", sessionStartHandler(pool, limiter)),
	)

	s.AddTool(
		mcplib.NewTool("session_end",
			mcplib.WithDescription("End a session. Idempotent — calling it on an already-ended session returns the same row. Returns ended_at and the node count attached to this session."),
			mcplib.WithString("session_id", mcplib.Required(), mcplib.Description("UUID of the session to end.")),
			mcplib.WithString("summary", mcplib.Description("Optional free-form summary (≤4096 chars). Stored in sessions.summary.")),
		),
		instrumentTool("session_end", sessionEndHandler(pool, limiter)),
	)

	s.AddTool(
		mcplib.NewTool("session_summary",
			mcplib.WithDescription("Return the full list of nodes (including soft-deleted) created during a session, ordered chronologically. Useful for building changelogs or audit trails after a task completes."),
			mcplib.WithString("session_id", mcplib.Required(), mcplib.Description("UUID of the session to summarise.")),
		),
		instrumentTool("session_summary", sessionSummaryHandler(pool)),
	)
}

// ── session_start ─────────────────────────────────────────────────────────────

type sessionStartArgs struct {
	Project    string `json:"project,omitempty"`
	WorkingDir string `json:"working_dir,omitempty"`
}

func sessionStartHandler(pool *pgxpool.Pool, limiter *ratelimit.Limiter) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if result := checkRateLimit(ctx, limiter); result != nil {
			return result, nil
		}

		var args sessionStartArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		// Validate project naming when provided (session.project_id is informational).
		if args.Project != "" {
			if verr := validate.Check(args.Project); verr != nil {
				return verr.ToMCPResult(), nil
			}
		}
		projectID := args.Project
		if projectID == "" {
			projectID = validate.DefaultProjectID
		}

		// Resolve optional user from auth context (NULL when MCP_AUTH=none).
		userID, _ := attributionFromContext(ctx)

		tx, err := pool.Begin(ctx)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}
		defer tx.Rollback(ctx) //nolint:errcheck

		sessionUUID, err := store.CreateSession(ctx, tx, userID, projectID, args.WorkingDir)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		if err := tx.Commit(ctx); err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{
			"session_id": sessionUUID,
			"started_at": time.Now().UTC().Format(time.RFC3339),
		}), nil
	}
}

// ── session_end ───────────────────────────────────────────────────────────────

type sessionEndArgs struct {
	SessionID string `json:"session_id"`
	Summary   string `json:"summary,omitempty"`
}

func sessionEndHandler(pool *pgxpool.Pool, limiter *ratelimit.Limiter) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if result := checkRateLimit(ctx, limiter); result != nil {
			return result, nil
		}

		var args sessionEndArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		if !isValidUUID(args.SessionID) {
			return errorResult("session_id must be a valid UUID"), nil
		}

		if len(args.Summary) > sessionSummaryMaxBytes {
			return errorResult(fmt.Sprintf("summary must be ≤%d characters", sessionSummaryMaxBytes)), nil
		}

		var summaryPtr *string
		if args.Summary != "" {
			summaryPtr = &args.Summary
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}
		defer tx.Rollback(ctx) //nolint:errcheck

		sess, err := store.EndSession(ctx, tx, args.SessionID, summaryPtr)
		if err == store.ErrSessionNotFound {
			verr := &validate.Error{
				Code:    validate.CodeSessionNotFound,
				Message: fmt.Sprintf("Sesión no encontrada: %s", args.SessionID),
				Layer:   validate.LayerSession,
			}
			return verr.ToMCPResult(), nil
		}
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		if err := tx.Commit(ctx); err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		nodeCount, err := store.CountSessionNodes(ctx, pool, sess.ID)
		if err != nil {
			return errorResult(fmt.Sprintf("db error counting nodes: %s", err)), nil
		}

		var endedAt string
		if sess.EndedAt != nil {
			endedAt = sess.EndedAt.UTC().Format(time.RFC3339)
		}

		return jsonResult(map[string]any{
			"session_id": sess.ID,
			"ended_at":   endedAt,
			"node_count": nodeCount,
		}), nil
	}
}

// ── session_summary ───────────────────────────────────────────────────────────

type sessionSummaryArgs struct {
	SessionID string `json:"session_id"`
}

func sessionSummaryHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		var args sessionSummaryArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		if !isValidUUID(args.SessionID) {
			return errorResult("session_id must be a valid UUID"), nil
		}

		sess, err := store.GetSession(ctx, pool, args.SessionID)
		if err == store.ErrSessionNotFound {
			verr := &validate.Error{
				Code:    validate.CodeSessionNotFound,
				Message: fmt.Sprintf("Sesión no encontrada: %s", args.SessionID),
				Layer:   validate.LayerSession,
			}
			return verr.ToMCPResult(), nil
		}
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		nodes, err := store.ListSessionNodes(ctx, pool, args.SessionID)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(buildSessionSummaryResponse(sess, nodes)), nil
	}
}

// sessionNodeJSON is the wire shape for one node in session_summary.nodes.
type sessionNodeJSON struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	NodeType         string  `json:"node_type"`
	ProjectID        string  `json:"project_id"`
	CreatedAt        string  `json:"created_at"`
	DeletedAt        *string `json:"deleted_at"`
	ObservationCount int     `json:"observation_count"`
}

func buildSessionSummaryResponse(sess *store.Session, nodes []store.SessionNodeRow) map[string]any {
	var endedAt *string
	if sess.EndedAt != nil {
		s := sess.EndedAt.UTC().Format(time.RFC3339)
		endedAt = &s
	}

	var workingDir *string
	if sess.WorkingDir != nil {
		workingDir = sess.WorkingDir
	}

	nodeList := make([]sessionNodeJSON, len(nodes))
	for i, n := range nodes {
		var deletedAt *string
		if n.DeletedAt != nil {
			s := n.DeletedAt.UTC().Format(time.RFC3339)
			deletedAt = &s
		}
		nodeList[i] = sessionNodeJSON{
			ID:               n.ID,
			Name:             n.Name,
			NodeType:         n.NodeType,
			ProjectID:        n.ProjectID,
			CreatedAt:        n.CreatedAt.UTC().Format(time.RFC3339),
			DeletedAt:        deletedAt,
			ObservationCount: n.ObservationCount,
		}
	}

	return map[string]any{
		"session_id":  sess.ID,
		"started_at":  sess.StartedAt.UTC().Format(time.RFC3339),
		"ended_at":    endedAt,
		"project_id":  sess.ProjectID,
		"working_dir": workingDir,
		"summary":     sess.Summary,
		"nodes":       nodeList,
	}
}
