package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// RegisterEntities registers the create_entities, add_observations,
// delete_entities, and delete_observations tools on the server.
func RegisterEntities(s *server.MCPServer, pool *pgxpool.Pool) {
	s.AddTool(
		mcplib.NewTool("create_entities",
			mcplib.WithDescription("Create new entities in the knowledge graph. Each entity must have name, entityType, and observations. Idempotent on name."),
			mcplib.WithArray("entities", mcplib.Required()),
		),
		createEntitiesHandler(pool),
	)

	s.AddTool(
		mcplib.NewTool("add_observations",
			mcplib.WithDescription("Add observations to existing entities. Each item must have entityName and contents (array of strings)."),
			mcplib.WithArray("observations", mcplib.Required()),
		),
		addObservationsHandler(pool),
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

func createEntitiesHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
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
		if verr := validate.Run(vp, validate.KindEntities); verr != nil {
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
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	createdEntities, createdObs := 0, 0
	for _, e := range entities {
		id, err := store.Create(ctx, tx, e.Name, e.EntityType)
		if err != nil {
			return 0, 0, fmt.Errorf("create entity %q: %w", e.Name, err)
		}
		createdEntities++

		for _, obsText := range e.Observations {
			_, inserted, err := store.Insert(ctx, tx, id, obsText)
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

// ── add_observations ─────────────────────────────────────────────────────────

type addObsInput struct {
	EntityName string   `json:"entityName"`
	Contents   []string `json:"contents"`
}

type addObservationsArgs struct {
	Observations []addObsInput `json:"observations"`
}

func addObservationsHandler(pool *pgxpool.Pool) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		var args addObservationsArgs
		if err := req.BindArguments(&args); err != nil {
			return errorResult(fmt.Sprintf("invalid arguments: %s", err)), nil
		}

		// Flatten into validate.Observation slice for the Content Filter.
		var flatObs []validate.Observation
		for _, item := range args.Observations {
			for _, text := range item.Contents {
				flatObs = append(flatObs, validate.Observation{EntityName: item.EntityName, Text: text})
			}
		}
		if verr := validate.Run(validate.Payload{Observations: flatObs}, validate.KindObservations); verr != nil {
			return verr.ToMCPResult(), nil
		}

		added, err := execAddObservations(ctx, pool, args.Observations)
		if err != nil {
			return errorResult(fmt.Sprintf("db error: %s", err)), nil
		}

		return jsonResult(map[string]any{"added": added}), nil
	}
}

func execAddObservations(ctx context.Context, pool *pgxpool.Pool, items []addObsInput) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	added := 0
	for _, item := range items {
		id, _, found, err := store.FindByName(ctx, tx, item.EntityName)
		if err != nil {
			return 0, fmt.Errorf("find entity %q: %w", item.EntityName, err)
		}
		if !found {
			return 0, &entityNotFoundError{name: item.EntityName}
		}

		for _, text := range item.Contents {
			_, inserted, err := store.Insert(ctx, tx, id, text)
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
