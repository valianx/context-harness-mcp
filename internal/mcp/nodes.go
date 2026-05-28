package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel/codes"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
	"github.com/mariogutierrez/context-harness-mcp/internal/observability"
	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// isValidUUID returns true when s is a lower-case UUID string.
func isValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
	}
	return true
}

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
			mcplib.WithDescription("Create new nodes in the knowledge graph. Each node must have name, nodeType, and observations. Idempotent on (project, name)."),
			mcplib.WithArray("nodes", mcplib.Required()),
		),
		instrumentTool("create_nodes", createNodesHandler(pool, limiter)),
	)

	s.AddTool(
		mcplib.NewTool("add_observations",
			mcplib.WithDescription("Add observations to existing nodes. Each item must have nodeName and contents (array of strings)."),
			mcplib.WithArray("observations", mcplib.Required()),
		),
		instrumentTool("add_observations", addObservationsHandler(pool, limiter)),
	)

	s.AddTool(
		mcplib.NewTool("update_observations",
			mcplib.WithDescription("Atomically replace an existing observation text with new text on a node. Each update must have nodeName, old_text (exact match), and new_text. old_text is soft-deleted and new_text is inserted with a fresh embedding in a single transaction. Rolls back entirely on any error."),
			mcplib.WithArray("updates", mcplib.Required()),
		),
		instrumentTool("update_observations", updateObservationsHandler(pool, limiter)),
	)
}

// ── create_nodes ──────────────────────────────────────────────────────────────

type createNodeInput struct {
	Name         string   `json:"name"`
	NodeType     string   `json:"nodeType"`
	Observations []string `json:"observations"`
	Project      string   `json:"project,omitempty"`
	SessionID    string   `json:"session_id,omitempty"`
}

type createNodesArgs struct {
	Nodes []createNodeInput `json:"nodes"`
}

func createNodesHandler(pool *pgxpool.Pool, limiter *ratelimit.Limiter) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		start := time.Now()
		ctx, span := toolTracer().Start(ctx, "mcp.create_nodes")
		defer span.End()

		span.SetAttributes(attrToolName.String("create_nodes"))

		// outcome is set by defer so all exit paths are covered (AC-E.5).
		outcome := outcomeServerError
		defer func() {
			span.SetAttributes(attrToolOutcome.String(outcome))
			observability.RecordRequest(ctx, "create_nodes", outcome, time.Since(start))
		}()

		if uid := auth.UserIDFromContext(ctx); uid != "" {
			span.SetAttributes(attrUserID.String(uid))
		}

		if result := checkRateLimit(ctx, limiter); result != nil {
			outcome = outcomePolicyReject
			return result, nil
		}

		var args createNodesArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		// Validate project naming BEFORE the Content Filter — project is a tag,
		// not content, so it does not go through the Content Filter layers.
		for i, n := range args.Nodes {
			if n.Project != "" {
				if verr := validate.Check(n.Project); verr != nil {
					outcome = outcomePolicyReject
					span.SetAttributes(
						attrErrorCode.String(verr.Code),
						attrLayer.String(verr.Layer),
					)
					return verr.ToMCPResult(), nil
				}
			}
			// Resolve default project so the rest of the handler can use it directly.
			if args.Nodes[i].Project == "" {
				args.Nodes[i].Project = validate.DefaultProjectID
			}
		}

		// Validate and resolve session_id (same value for all nodes in the batch).
		// We take the session_id from the first node that provides one; within a
		// single call all nodes share the same session (or no session at all).
		var sessionID *string
		for _, n := range args.Nodes {
			if n.SessionID != "" {
				if !isValidUUID(n.SessionID) {
					return errorResult("session_id must be a valid UUID"), nil
				}
				sid := n.SessionID
				sessionID = &sid
				break
			}
		}
		if sessionID != nil {
			sess, err := store.GetSession(ctx, pool, *sessionID)
			if err == store.ErrSessionNotFound {
				verr := &validate.Error{
					Code:    validate.CodeSessionNotFound,
					Message: fmt.Sprintf("Sesión no encontrada: %s", *sessionID),
					Layer:   validate.LayerSession,
				}
				outcome = outcomePolicyReject
				span.SetAttributes(
					attrErrorCode.String(verr.Code),
					attrLayer.String(verr.Layer),
				)
				return verr.ToMCPResult(), nil
			}
			if err != nil {
				span.SetStatus(codes.Error, err.Error())
				return errorResult(fmt.Sprintf("db error: %s", err)), nil
			}
			if sess.EndedAt != nil {
				verr := &validate.Error{
					Code:    validate.CodeSessionAlreadyEnded,
					Message: fmt.Sprintf("La sesión %s ya ha finalizado.", *sessionID),
					Layer:   validate.LayerSession,
				}
				outcome = outcomePolicyReject
				span.SetAttributes(
					attrErrorCode.String(verr.Code),
					attrLayer.String(verr.Layer),
				)
				return verr.ToMCPResult(), nil
			}
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
			outcome = outcomePolicyReject
			span.SetStatus(codes.Error, verr.Code)
			span.SetAttributes(
				attrErrorCode.String(verr.Code),
				attrLayer.String(verr.Layer),
			)
			observability.RecordValidationRejection(ctx, verr.Layer, verr.MatchedPattern)
			return verr.ToMCPResult(), nil
		}

		// Count metadata only — no content in span attributes (AC-E.3).
		totalObs := 0
		for _, n := range args.Nodes {
			totalObs += len(n.Observations)
		}
		span.SetAttributes(
			attrNodeCount.Int(len(args.Nodes)),
			attrObservationCount.Int(totalObs),
			attrHasEmbedding.Bool(totalObs > 0),
		)

		userID, email := attributionFromContext(ctx)
		createdNodes, createdObs, err := execCreateNodes(ctx, pool, args.Nodes, sessionID, userID, email)
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		outcome = outcomeSuccess
		return jsonResult(map[string]any{
			"created_nodes":        createdNodes,
			"created_observations": createdObs,
		}), nil
	}
}

func execCreateNodes(ctx context.Context, pool *pgxpool.Pool, nodes []createNodeInput, sessionID *string, userID, email *string) (int, int, error) {
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
		id, err := store.Create(ctx, tx, n.Name, n.NodeType, n.Project, sessionID, userID, email)
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
		start := time.Now()
		ctx, span := toolTracer().Start(ctx, "mcp.add_observations")
		defer span.End()

		span.SetAttributes(attrToolName.String("add_observations"))

		outcome := outcomeServerError
		defer func() {
			span.SetAttributes(attrToolOutcome.String(outcome))
			observability.RecordRequest(ctx, "add_observations", outcome, time.Since(start))
		}()

		if uid := auth.UserIDFromContext(ctx); uid != "" {
			span.SetAttributes(attrUserID.String(uid))
		}

		if result := checkRateLimit(ctx, limiter); result != nil {
			outcome = outcomePolicyReject
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
			outcome = outcomePolicyReject
			span.SetStatus(codes.Error, verr.Code)
			span.SetAttributes(
				attrErrorCode.String(verr.Code),
				attrLayer.String(verr.Layer),
			)
			observability.RecordValidationRejection(ctx, verr.Layer, verr.MatchedPattern)
			return verr.ToMCPResult(), nil
		}

		// Count metadata only — no content in span attributes (AC-E.3).
		span.SetAttributes(
			attrObservationCount.Int(len(flatObs)),
			attrHasEmbedding.Bool(len(flatObs) > 0),
		)

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
			span.SetStatus(codes.Error, err.Error())
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		outcome = outcomeSuccess
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
		// Derive project from the node — no project param on add_observations.
		// nil filter: return first match by project_id ASC if homonyms exist.
		id, _, _, found, err := store.FindByName(ctx, tx, item.NodeName, nil)
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

// ── update_observations ───────────────────────────────────────────────────────

type updateObsInput struct {
	NodeName string `json:"nodeName"`
	OldText  string `json:"old_text"`
	NewText  string `json:"new_text"`
}

type updateObservationsArgs struct {
	Updates []updateObsInput `json:"updates"`
}

func updateObservationsHandler(pool *pgxpool.Pool, limiter *ratelimit.Limiter) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if result := checkRateLimit(ctx, limiter); result != nil {
			return result, nil
		}

		var args updateObservationsArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		if len(args.Updates) == 0 {
			return errorResult("updates must be non-empty"), nil
		}

		// Validate old_text != new_text before opening any Tx. This is a
		// logical no-op guard: replacing text with itself serves no purpose and
		// would create a (node_id, text) conflict against the row we're about
		// to soft-delete (depending on Tx ordering vs. partial index presence).
		for _, u := range args.Updates {
			if u.OldText == u.NewText {
				return errorResult("new_text identical to old_text"), nil
			}
		}

		// Run the Content Filter over new_text values before any Tx.
		// old_text is a lookup key only — it does not pass through the filter.
		flatObs := make([]validate.Observation, len(args.Updates))
		for i, u := range args.Updates {
			flatObs[i] = validate.Observation{NodeName: u.NodeName, Text: u.NewText}
		}
		vp := validate.Payload{Observations: flatObs}
		if verr := validate.Run(&vp, validate.KindObservations); verr != nil {
			return verr.ToMCPResult(), nil
		}

		// Propagate potentially-redacted texts back into args (SECRET_MODE=redact path).
		for i := range args.Updates {
			args.Updates[i].NewText = vp.Observations[i].Text
		}

		userID, email := attributionFromContext(ctx)
		updated, err := execUpdateObservations(ctx, pool, args.Updates, userID, email)
		if err != nil {
			return errorResult(err.Error()), nil
		}

		return jsonResult(map[string]any{"updated": updated}), nil
	}
}

func execUpdateObservations(ctx context.Context, pool *pgxpool.Pool, updates []updateObsInput, userID, email *string) (int, error) {
	// Truncate and batch-embed all new texts before opening any Tx.
	// This avoids wasting a DB transaction on a model failure.
	refs := make([]obsRef, len(updates))
	for i, u := range updates {
		truncated := embed.TruncateToTokens(u.NewText, 256)
		updates[i].NewText = truncated
		refs[i] = obsRef{nodeIdx: i, obsIdx: 0, text: truncated}
	}

	embeddings, err := batchEmbed(ctx, refs)
	if err != nil {
		return 0, fmt.Errorf("embed encode: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	for i, u := range updates {
		// Derive project from the node — no project param on update_observations.
		// nil filter: return first match by project_id ASC if homonyms exist.
		nodeID, _, _, found, err := store.FindByName(ctx, tx, u.NodeName, nil)
		if err != nil {
			return 0, fmt.Errorf("find node %q: %w", u.NodeName, err)
		}
		if !found {
			return 0, fmt.Errorf("node not found: %s", u.NodeName)
		}

		count, err := store.MarkObservationDeletedByText(ctx, tx, nodeID, u.OldText)
		if err != nil {
			return 0, fmt.Errorf("soft-delete observation for %q: %w", u.NodeName, err)
		}
		if count == 0 {
			return 0, fmt.Errorf("observation not found: nodeName=%s", u.NodeName)
		}

		vec := embeddings[[2]int{i, 0}]
		if _, _, err := store.Insert(ctx, tx, nodeID, u.NewText, vec, userID, email); err != nil {
			return 0, fmt.Errorf("insert new observation for %q: %w", u.NodeName, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(updates), nil
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
