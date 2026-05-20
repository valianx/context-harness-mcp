package khctl

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SeedNode holds the fixture data for a single KG node.
type SeedNode struct {
	Name         string
	NodeType     string
	Observations []string
}

// SeedRelation holds the fixture data for a single KG relation.
type SeedRelation struct {
	From         string
	To           string
	RelationType string
}

// FixtureNodes defines the deterministic dev fixtures: ≥20 nodes across ≥5
// node types, ≥3 observations each. Used by `khctl seed` to populate a fresh
// KG with reference content. Platform-agnostic: no specific hosting provider
// is privileged.
var FixtureNodes = []SeedNode{
	// pattern (5 nodes)
	{
		Name:     "jwt-auth-pattern",
		NodeType: "pattern",
		Observations: []string{
			"Use short-lived JWTs (15 min access token + 7-day refresh token) to limit blast radius of token theft.",
			"Store refresh tokens in HttpOnly Secure cookies, never in localStorage or sessionStorage.",
			"Include jti (JWT ID) claim and maintain a server-side blocklist for revocation before expiry.",
			"Sign with RS256 (asymmetric) for services that need to verify without the signing secret.",
		},
	},
	{
		Name:     "rate-limiting-pattern",
		NodeType: "pattern",
		Observations: []string{
			"Apply rate limits at the API gateway layer before requests reach service handlers.",
			"Use a sliding-window algorithm (Redis + token bucket) for per-IP and per-user-ID limits.",
			"Return 429 with Retry-After header; never expose internal queue depth in the response.",
			"Separate limits for read vs write endpoints — writes carry higher abuse risk.",
		},
	},
	{
		Name:     "idempotent-write-pattern",
		NodeType: "pattern",
		Observations: []string{
			"Idempotency keys (client-supplied UUID in header) let the client retry safely on network failure.",
			"Store the idempotency key and its response in Redis with a 24-hour TTL.",
			"ON CONFLICT DO NOTHING is the Postgres primitive for idempotent INSERT pipelines.",
			"Log deduplication hits separately from primary write metrics to detect client retry storms.",
		},
	},
	{
		Name:     "soft-delete-pattern",
		NodeType: "pattern",
		Observations: []string{
			"Set deleted_at = now() instead of DELETE to preserve audit history and enable recovery.",
			"Every read query filters WHERE deleted_at IS NULL; use partial indexes to keep scans fast.",
			"Expose a hard-delete admin endpoint only to service accounts — never to end users.",
			"Weekly pg_dump covers the 7-day recovery window for accidental soft-deletes.",
		},
	},
	{
		Name:     "vector-search-pattern",
		NodeType: "pattern",
		Observations: []string{
			"HNSW index with vector_cosine_ops gives sub-millisecond ANN search at ≤1 million vectors.",
			"Embed at query time with the same model used at insert time — model mismatch silently returns wrong results.",
			"Aggregate per-entity with MIN(distance) when observations are stored at row granularity.",
			"Use ef_search=64 for recall=0.95 trade-off; bump to 128 if precision degrades at scale.",
		},
	},
	// decision (4 nodes)
	{
		Name:     "go-for-mcp-server-runtime",
		NodeType: "decision",
		Observations: []string{
			"Go static binary starts in 1-3 s, keeping cold-starts viable on free-tier container hosting.",
			"fastembed-go provides all-MiniLM-L6-v2 ONNX inference natively — no Python sidecar needed.",
			"Single-binary deployment: no interpreter, no virtualenv, no language runtime mismatch between dev and prod.",
			"Trade-off: smaller ecosystem of MCP examples in Go vs Python; offset by simpler ops surface.",
		},
	},
	{
		Name:     "single-tenant-schema-v1",
		NodeType: "decision",
		Observations: []string{
			"No tenant_id column in v1 — single team or single deployer per instance, YAGNI for multi-tenancy.",
			"Adding tenant_id later is an additive migration: add column → backfill → switch queries → drop old default.",
			"Postgres-level access control (role grants) pins write access to the configured DATABASE_URL only.",
			"Single-tenant keeps the query surface minimal and the indexes tight.",
		},
	},
	{
		Name:     "auth-opt-in-via-mcp-auth-env",
		NodeType: "decision",
		Observations: []string{
			"Single env var MCP_AUTH=none|enabled toggles between fully-open (local-trusted) and bearer-required (shared/remote) modes.",
			"Default `none` keeps single-developer local deployments friction-free — no JWT, no setup, just docker compose up.",
			"`enabled` mode plugs in an identity provider (Supabase Auth, Auth0, custom JWT issuer) via standard JWT validation.",
			"Same image and binary across both modes — runtime config decides; no separate builds for trusted vs untrusted environments.",
		},
	},
	{
		Name:     "containerized-deployment-strategy",
		NodeType: "decision",
		Observations: []string{
			"Single Docker image deploys identically to local (docker compose), cloud container hosts, and self-hosted servers.",
			"Free-tier viable: Postgres+pgvector + container host on free plans serves a single team at low-MB data sizes.",
			"Operator-controlled backups: weekly pg_dump via CI replaces managed PITR when not available.",
			"Idle-pause mitigation when present: 6-day SELECT 1 keepalive cron via CI prevents free-tier auto-pause.",
		},
	},
	// stack-profile (4 nodes)
	// Note: "context-harness-mcp" also appears as project type below; DB unique
	// constraint is on name alone so only the first occurrence is inserted.
	{
		Name:     "context-harness-mcp",
		NodeType: "stack-profile",
		Observations: []string{
			"Go 1.23 + mcp-go (mark3labs) + pgx/v5 + pgvector-go + fastembed-go (ONNX all-MiniLM-L6-v2 384 dims).",
			"Deployed as a multi-stage Docker image; runs anywhere Docker + Postgres+pgvector is available (local laptop, container hosts, self-hosted).",
			"MCP transport: streamable-http (March 2025 spec revision); local dev uses stdio.",
			"Content Filter: three layers — syntactic (size + junk denylist), secrets (gitleaks + regex), taxonomy (enum + path rejection).",
		},
	},
	{
		Name:     "two-deployment-modes-stack",
		NodeType: "stack-profile",
		Observations: []string{
			"Local mode: docker compose up + MCP_AUTH=none (default). Same image, no JWT, ideal for single-developer use.",
			"Cloud/shared mode: any container host + Postgres+pgvector + MCP_AUTH=enabled + JWT bearer in ~/.claude.json.",
			"Both modes are first-class — neither is a degraded version of the other. Mode is chosen per-deployment via env var.",
			"The MCP wire protocol, the tools surface, and the DB schema are identical across modes.",
		},
	},
	{
		Name:     "github-actions-ci-cd-stack",
		NodeType: "stack-profile",
		Observations: []string{
			"ci.yml: go build + go vet + staticcheck + go test (testcontainers, ubuntu-latest, Docker available).",
			"deploy.yml: goose up against the configured DATABASE_URL, then optional DEPLOY_HOOK_URL trigger (works with Railway/Render/Fly/Coolify/custom CI).",
			"pg_dump_weekly.yml: Sunday 03:00 UTC encrypted dump (gpg --symmetric AES-256) retained 90 days.",
			"keepalive workflow: SELECT 1 every 6 days for hosting providers that auto-pause idle databases (no-op when not needed).",
		},
	},
	{
		Name:     "pgvector-embedding-stack",
		NodeType: "stack-profile",
		Observations: []string{
			"pgvector extension on Postgres 16 — vector(384) column on observations table.",
			"HNSW index with vector_cosine_ops (m=16, ef_construction=64) on observations.embedding WHERE deleted_at IS NULL.",
			"pgvector-go v0.2.2 for type registration in pgxpool AfterConnect hook.",
			"Cosine distance query via <=> operator; aggregate per-entity with MIN(distance) in search_nodes.",
		},
	},
	// service (4 nodes)
	{
		Name:     "mcp-server-deployment",
		NodeType: "service",
		Observations: []string{
			"Container running the Go binary, listening on $PORT (default :7654 for local runs) with /mcp/ + /auth/* + /viewer/* endpoints.",
			"Cold-start budget 1-3 s — viable on platforms that idle/sleep containers (most free tiers).",
			"Healthcheck at /healthz: returns 200 with {status:ok, db:...} once the DB pool is ready.",
			"Same Dockerfile across all deployments — runtime configuration is via env vars only (no per-platform image variants).",
		},
	},
	{
		Name:     "postgres-pgvector-service",
		NodeType: "service",
		Observations: []string{
			"Postgres 16+ with the pgvector extension. Supabase/Neon/AWS RDS/self-hosted — all work; the schema is portable.",
			"Connection exposed as DATABASE_URL; the server fails fast at boot if unset.",
			"Free-tier providers typically cap DB size and may auto-pause; keepalive cron mitigates the latter.",
			"Weekly pg_dump artifact retained via CI; restore via gpg --decrypt | psql when needed.",
		},
	},
	{
		Name:     "mcp-server-healthz-endpoint",
		NodeType: "service",
		Observations: []string{
			"GET /healthz returns 200 {status:ok, db:ok} when the DB pool's SELECT 1 succeeds.",
			"Returns 503 if DB is unreachable — used by container host healthchecks and smoke scripts.",
			"Does not load the ONNX model; embedding health is inferred from the first successful search_nodes.",
			"Smoke test happy_path.sh calls /healthz first and aborts if it returns non-200.",
		},
	},
	{
		Name:     "claude-code-mcp-client",
		NodeType: "service",
		Observations: []string{
			"Claude Code (and any MCP-compatible client) reads ~/.claude.json for MCP server registration under mcpServers.",
			"Local mode: type http, url http://localhost:7654/mcp/ — no headers needed (MCP_AUTH=none).",
			"Cloud/shared mode: type http, url https://<your-host>/mcp/ + headers.Authorization Bearer <jwt> (MCP_AUTH=enabled).",
			"The snippet that lands the bearer in ~/.claude.json is generated by the /auth/exchange endpoint after a magic-link login.",
		},
	},
	// project (2 unique names)
	{
		Name:     "mcp-compatible-clients",
		NodeType: "project",
		Observations: []string{
			"Any client speaking the MCP HTTP transport (Claude Code, Cursor, custom integrations) can consume the server.",
			"The client config sits in the client's own settings file — typically a JSON entry with {type:http, url, headers?} shape.",
			"Headers carry an optional bearer JWT when MCP_AUTH=enabled; omitted when MCP_AUTH=none.",
			"No client-side library required — the server speaks plain HTTP + JSON-RPC over a single endpoint.",
		},
	},
	{
		Name:     "operator-backups",
		NodeType: "project",
		Observations: []string{
			"Operator-controlled: GH Actions workflow stores weekly encrypted pg_dump artifacts.",
			"Artifacts retained 90 days via GH Actions retention policy (operator can lengthen via dedicated artifact storage).",
			"Restore: gpg --decrypt < dump.sql.gpg | psql $DATABASE_URL.",
			"DUMP_PASSPHRASE stored in GH secrets; rotate via openssl rand -base64 32.",
		},
	},
	// constraint (2 nodes — total ≥ 20 unique names)
	{
		Name:     "embedding-model-locked-at-384-dims",
		NodeType: "constraint",
		Observations: []string{
			"all-MiniLM-L6-v2 produces 384-dim vectors; locked for the lifetime of the deployment.",
			"Changing the model later requires a full re-embed of the entire KG — a separate operation.",
			"Cosine distance is the metric across both index and queries; switching metrics requires reindex.",
			"Fixture JSONs and import payloads must use 384-dim float32 arrays; khctl import rejects any other dimensionality.",
		},
	},
	{
		Name:     "content-filter-three-layer-contract",
		NodeType: "constraint",
		Observations: []string{
			"Every write payload crosses three layers before INSERT: syntactic, secrets, taxonomy.",
			"Rejection at any layer aborts the entire call — no partial writes.",
			"Error codes are stable: policy/size-exceeded, policy/junk-pattern, policy/secret-detected, policy/taxonomy-violation.",
			"Error messages are in Spanish so Claude surfaces them to the user without re-translation.",
		},
	},
}

// FixtureRelations defines ≥10 deterministic relations crossing multiple node types.
var FixtureRelations = []SeedRelation{
	{From: "context-harness-mcp", To: "pgvector-embedding-stack", RelationType: "uses-stack"},
	{From: "context-harness-mcp", To: "github-actions-ci-cd-stack", RelationType: "uses-stack"},
	{From: "context-harness-mcp", To: "two-deployment-modes-stack", RelationType: "uses-stack"},
	{From: "context-harness-mcp", To: "mcp-server-deployment", RelationType: "belongs-to"},
	{From: "context-harness-mcp", To: "postgres-pgvector-service", RelationType: "depends-on"},
	{From: "mcp-server-deployment", To: "postgres-pgvector-service", RelationType: "depends-on"},
	{From: "claude-code-mcp-client", To: "mcp-server-deployment", RelationType: "calls"},
	{From: "context-harness-mcp", To: "go-for-mcp-server-runtime", RelationType: "relates_to"},
	{From: "context-harness-mcp", To: "containerized-deployment-strategy", RelationType: "relates_to"},
	{From: "context-harness-mcp", To: "single-tenant-schema-v1", RelationType: "relates_to"},
	{From: "context-harness-mcp", To: "auth-opt-in-via-mcp-auth-env", RelationType: "relates_to"},
	{From: "vector-search-pattern", To: "pgvector-embedding-stack", RelationType: "relates_to"},
	{From: "jwt-auth-pattern", To: "rate-limiting-pattern", RelationType: "relates_to"},
	{From: "soft-delete-pattern", To: "content-filter-three-layer-contract", RelationType: "relates_to"},
	{From: "operator-backups", To: "postgres-pgvector-service", RelationType: "belongs-to"},
	{From: "embedding-model-locked-at-384-dims", To: "pgvector-embedding-stack", RelationType: "relates_to"},
	{From: "mcp-server-healthz-endpoint", To: "mcp-server-deployment", RelationType: "belongs-to"},
	{From: "context-harness-mcp", To: "content-filter-three-layer-contract", RelationType: "depends-on"},
	{From: "mcp-compatible-clients", To: "context-harness-mcp", RelationType: "calls"},
}

// RunSeed executes the full seed inside a single transaction. Returns counts of
// inserted rows (nodesInserted, obsInserted, relsInserted).
func RunSeed(ctx context.Context, pool *pgxpool.Pool, reset bool) (nodesIn, obsIn, relsIn int, err error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	if reset {
		if _, execErr := tx.Exec(ctx,
			"TRUNCATE TABLE relations, observations, nodes RESTART IDENTITY CASCADE"); execErr != nil {
			err = fmt.Errorf("truncate: %w", execErr)
			return
		}
	}

	nodesIn, err = insertFixtureNodes(ctx, tx)
	if err != nil {
		return
	}
	obsIn, err = insertFixtureObservations(ctx, tx)
	if err != nil {
		return
	}
	relsIn, err = insertFixtureRelations(ctx, tx)
	if err != nil {
		return
	}

	err = tx.Commit(ctx)
	return
}

// insertFixtureNodes inserts all fixture nodes using ON CONFLICT DO NOTHING.
// Duplicate names are skipped at the Go level before reaching the DB.
func insertFixtureNodes(ctx context.Context, tx pgx.Tx) (int, error) {
	seen := make(map[string]struct{}, len(FixtureNodes))
	inserted := 0
	for _, n := range FixtureNodes {
		if _, dup := seen[n.Name]; dup {
			continue
		}
		seen[n.Name] = struct{}{}

		// Seed fixtures all land in the 'global' project. The composite UNIQUE
		// constraint nodes_project_name_key (project_id, name) replaced the
		// historical entities_name_key (name) UNIQUE in migration 00007.
		var id string
		err := tx.QueryRow(ctx,
			`INSERT INTO nodes (name, node_type, project_id)
			 VALUES ($1, $2, 'global')
			 ON CONFLICT ON CONSTRAINT nodes_project_name_key DO NOTHING
			 RETURNING id`,
			n.Name, n.NodeType,
		).Scan(&id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return inserted, fmt.Errorf("insert node %q: %w", n.Name, err)
		}
		inserted++
	}
	return inserted, nil
}

// insertFixtureObservations inserts observations for all fixture nodes.
func insertFixtureObservations(ctx context.Context, tx pgx.Tx) (int, error) {
	seen := make(map[string]struct{}, len(FixtureNodes))
	inserted := 0
	for _, n := range FixtureNodes {
		if _, dup := seen[n.Name]; dup {
			continue
		}
		seen[n.Name] = struct{}{}

		var nodeID string
		if err := tx.QueryRow(ctx,
			`SELECT id FROM nodes WHERE name = $1 AND deleted_at IS NULL`, n.Name,
		).Scan(&nodeID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return inserted, fmt.Errorf("resolve node %q: %w", n.Name, err)
		}

		for _, text := range n.Observations {
			var id string
			err := tx.QueryRow(ctx,
				`INSERT INTO observations (node_id, text)
				 VALUES ($1, $2)
				 ON CONFLICT (node_id, text) DO NOTHING
				 RETURNING id`,
				nodeID, text,
			).Scan(&id)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				return inserted, fmt.Errorf("insert observation for node %q: %w", n.Name, err)
			}
			inserted++
		}
	}
	return inserted, nil
}

// insertFixtureRelations inserts all fixture relations. Silently skips relations
// whose from/to nodes are absent.
func insertFixtureRelations(ctx context.Context, tx pgx.Tx) (int, error) {
	inserted := 0
	for _, r := range FixtureRelations {
		var fromID, toID string
		if err := tx.QueryRow(ctx,
			`SELECT id FROM nodes WHERE name = $1 AND deleted_at IS NULL`, r.From,
		).Scan(&fromID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return inserted, fmt.Errorf("resolve from-node %q: %w", r.From, err)
		}
		if err := tx.QueryRow(ctx,
			`SELECT id FROM nodes WHERE name = $1 AND deleted_at IS NULL`, r.To,
		).Scan(&toID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return inserted, fmt.Errorf("resolve to-node %q: %w", r.To, err)
		}

		var id string
		err := tx.QueryRow(ctx,
			`INSERT INTO relations (from_node_id, to_node_id, relation_type)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (from_node_id, to_node_id, relation_type) DO NOTHING
			 RETURNING id`,
			fromID, toID, r.RelationType,
		).Scan(&id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			return inserted, fmt.Errorf("insert relation %q→%q: %w", r.From, r.To, err)
		}
		inserted++
	}
	return inserted, nil
}
