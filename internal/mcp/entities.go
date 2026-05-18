package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	"github.com/mariogutierrez/context-harness-mcp/internal/embed"
	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// RegisterEntities registers the create_entities, add_observations,
// delete_entities, and delete_observations tools on the server.
// limiter enforces per-IP write-tool rate limits on create_entities and
// add_observations. Reads and deletes are unconstrained.
func RegisterEntities(s *server.MCPServer, pool *pgxpool.Pool, limiter *ratelimit.Limiter) {
	s.AddTool(
		mcplib.NewTool("create_entities",
			mcplib.WithDescription("Create new entities in the knowledge graph. Each entity must have name, entityType, and observations. Idempotent on name."),
			mcplib.WithArray("entities", mcplib.Required()),
		),
		createEntitiesHandler(pool, limiter),
	)

	s.AddTool(
		mcplib.NewTool("add_observations",
			mcplib.WithDescription("Add observations to existing entities. Each item must have entityName and contents (array of strings)."),
			mcplib.WithArray("observations", mcplib.Required()),
		),
		addObservationsHandler(pool, limiter),
	)

	s.AddTool(
		mcplib.NewTool("delete_entities",
			mcplib.WithDescription("Soft-delete entities by name. Related rows are kept for audit; deleted_at is set."),
			mcplib.WithArray("entityNames", mcplib.Required()),
		),
		deleteEntitiesHandler(pool),
	)

	s.AddTool(
		mcplib.NewTool("delete_observations",
			mcplib.WithDescription("Remove specific observations from entities by text match."),
			mcplib.WithArray("deletions", mcplib.Required()),
		),
		deleteObservationsHandler(pool),
	)
}

// ── create_entities ──────────────────────────────────────────────────────────

type createEntityInput struct {
	Name         string   `json:"name"`
	EntityType   string   `json:"entityType"`
	Observations []string `json:"observations"`
}

type createEntitiesArgs struct {
	Entities []createEntityInput `json:"entities"`
}

func createEntitiesHandler(pool *pgxpool.Pool, limiter *ratelimit.Limiter) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if result := checkRateLimit(ctx, limiter); result != nil {
			return result, nil
		}

		var args createEntitiesArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		// Build validate.Payload and run the Content Filter before opening any Tx.
		vp := validate.Payload{Entities: make([]validate.Entity, len(args.Entities))}
		for i, e := range args.Entities {
			vp.Entities[i] = validate.Entity{
				Name:         e.Name,
				EntityType:   e.EntityType,
				Observations: e.Observations,
			}
		}
		if verr := validate.Run(&vp, validate.KindEntities); verr != nil {
			return verr.ToMCPResult(), nil
		}

		createdEntities, createdObs, err := execCreateEntities(ctx, pool, args.Entities)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{
			"created_entities":     createdEntities,
			"created_observations": createdObs,
		}), nil
	}
}

func execCreateEntities(ctx context.Context, pool *pgxpool.Pool, entities []createEntityInput) (int, int, error) {
	// Collect all observation texts for batch embedding before opening any Tx.
	// This ensures a model failure does not waste a DB transaction.
	var allObs []obsRef
	for ei, e := range entities {
		for oi, text := range e.Observations {
			truncated := embed.TruncateToTokens(text, 256)
			entities[ei].Observations[oi] = truncated
			allObs = append(allObs, obsRef{entityIdx: ei, obsIdx: oi, text: truncated})
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

	createdEntities, createdObs := 0, 0
	for ei, e := range entities {
		id, err := store.Create(ctx, tx, e.Name, e.EntityType)
		if err != nil {
			return 0, 0, fmt.Errorf("create entity %q: %w", e.Name, err)
		}
		createdEntities++

		for oi, obsText := range e.Observations {
			vec := embeddingsByObs[[2]int{ei, oi}]
			_, inserted, err := store.Insert(ctx, tx, id, obsText, vec)
			if err != nil {
				return 0, 0, fmt.Errorf("insert observation for %q: %w", e.Name, err)
			}
			if inserted {
				createdObs++
			}
		}
	}

	return createdEntities, createdObs, tx.Commit(ctx)
}

// obsRef identifies a single observation by its position in the input batch.
type obsRef struct {
	entityIdx int
	obsIdx    int
	text      string
}

// batchEmbed encodes all observation texts in one call and returns a map
// keyed by [entityIdx, obsIdx] → pgvector.Vector. A model failure is returned
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
		result[[2]int{r.entityIdx, r.obsIdx}] = pgvector.NewVector(vecs[i])
	}
	return result, nil
}

// ── add_observations ─────────────────────────────────────────────────────────

type addObsInput struct {
	EntityName string   `json:"entityName"`
	Contents   []string `json:"contents"`
}

type addObservationsArgs struct {
	Observations []addObsInput `json:"observations"`
}

func addObservationsHandler(pool *pgxpool.Pool, limiter *ratelimit.Limiter) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		if result := checkRateLimit(ctx, limiter); result != nil {
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
				flatObs = append(flatObs, validate.Observation{EntityName: item.EntityName, Text: text})
			}
		}
		vp2 := validate.Payload{Observations: flatObs}
		if verr := validate.Run(&vp2, validate.KindObservations); verr != nil {
			return verr.ToMCPResult(), nil
		}

		// Propagate potentially-redacted texts back into args so the DB write
		// stores the scrubbed version rather than the original.
		flatIdx := 0
		for ii := range args.Observations {
			for ci := range args.Observations[ii].Contents {
				args.Observations[ii].Contents[ci] = vp2.Observations[flatIdx].Text
				flatIdx++
			}
		}

		added, err := execAddObservations(ctx, pool, args.Observations)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{"added": added}), nil
	}
}

func execAddObservations(ctx context.Context, pool *pgxpool.Pool, items []addObsInput) (int, error) {
	// Collect and truncate all texts; batch-encode before opening any Tx.
	var allObs []obsRef
	for ii, item := range items {
		for oi, text := range item.Contents {
			truncated := embed.TruncateToTokens(text, 256)
			items[ii].Contents[oi] = truncated
			allObs = append(allObs, obsRef{entityIdx: ii, obsIdx: oi, text: truncated})
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
		id, _, found, err := store.FindByName(ctx, tx, item.EntityName)
		if err != nil {
			return 0, fmt.Errorf("find entity %q: %w", item.EntityName, err)
		}
		if !found {
			return 0, &entityNotFoundError{name: item.EntityName}
		}

		for oi, text := range item.Contents {
			vec := embeddingsByObs[[2]int{ii, oi}]
			_, inserted, err := store.Insert(ctx, tx, id, text, vec)
			if err != nil {
				return 0, fmt.Errorf("insert observation for %q: %w", item.EntityName, err)
			}
			if inserted {
				added++
			}
		}
	}

	return added, tx.Commit(ctx)
}

// ── delete_entities ──────────────────────────────────────────────────────────

type deleteEntitiesArgs struct {
	EntityNames []string `json:"entityNames"`
}

func deleteEntitiesHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		var args deleteEntitiesArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		// No Content Filter for delete operations — these carry entity names, not user content.
		tx, err := pool.Begin(ctx)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}
		defer tx.Rollback(ctx) //nolint:errcheck

		deleted, err := store.MarkDeletedByNames(ctx, tx, args.EntityNames)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}
		if err := tx.Commit(ctx); err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{"deleted": deleted}), nil
	}
}

// ── delete_observations ──────────────────────────────────────────────────────

type deleteObsItem struct {
	EntityName   string   `json:"entityName"`
	Observations []string `json:"observations"`
}

type deleteObservationsArgs struct {
	Deletions []deleteObsItem `json:"deletions"`
}

func deleteObservationsHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		var args deleteObservationsArgs
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
		for _, item := range args.Deletions {
			id, _, found, err := store.FindByName(ctx, tx, item.EntityName)
			if err != nil {
				return errorResult(fmt.Sprintf("db error: %s", err)), nil
			}
			if !found {
				continue // entity already gone — skip silently
			}

			n, err := store.MarkDeletedByEntityAndTexts(ctx, tx, id, item.Observations)
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

// ── shared helpers ───────────────────────────────────────────────────────────

// entityNotFoundError is returned by add_observations when an entity name
// cannot be resolved. It is treated as a non-policy error (no Content Filter
// involvement) and surfaces as a structured JSON error result.
type entityNotFoundError struct {
	name string
}

func (e *entityNotFoundError) Error() string {
	return fmt.Sprintf("entity not found: %s", e.name)
}

// checkRateLimit reads the client IP from ctx and checks the write-tool rate
// limit. Returns a non-nil *mcp.CallToolResult (IsError=true) when the IP is
// over quota, or nil when the call is allowed to proceed.
//
// When limiter is nil or the context carries no IP (stdio transport), rate
// limiting is skipped and nil is returned.
func checkRateLimit(ctx context.Context, limiter *ratelimit.Limiter) *mcplib.CallToolResult {
	if limiter == nil {
		return nil
	}
	ip := ratelimit.IPFromContext(ctx)
	if ip == "" {
		// stdio transport — no IP, no rate limit.
		return nil
	}
	allowed, retryAfter := limiter.Allow(ip)
	if allowed {
		return nil
	}
	retrySecs := int(math.Ceil(retryAfter.Seconds()))
	payload, _ := json.Marshal(map[string]any{
		"code":                validate.CodeRateLimited,
		"message":             fmt.Sprintf("Demasiadas escrituras desde esta IP. Reintentar en %d segundos.", retrySecs),
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
