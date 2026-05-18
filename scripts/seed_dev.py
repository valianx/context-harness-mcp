#!/usr/bin/env python3
"""Seed a local Postgres with deterministic KG fixtures for development.

WARNING: This script is for development only. Do NOT run it against
Supabase production or any shared environment — it inserts synthetic
fixtures that may pollute the team knowledge graph.

Seeds ≥20 entities across ≥5 entity types, ≥10 relations, and ≥3
observations per entity. Content is realistic KG-shaped text suitable for
testing search_nodes("authentication patterns") and similar queries.

Embeddings are left NULL — the MCP server computes them on first access
through create_entities / add_observations. For embedding tests, use the
Go integration suite (tests/migration_test.go) which operates on a test
container with known embeddings from the fixture JSON.

Idempotency: without --reset, re-running is a no-op because every entity
INSERT uses ON CONFLICT (name) DO NOTHING.

Usage:
    uv run scripts/seed_dev.py [--dsn URL] [--reset]

Environment:
    SUPABASE_DB_URL  Postgres DSN used when --dsn is omitted.

Exit codes:
    0  success
    1  any error (DSN failure, invalid entity type, etc.)
"""
from __future__ import annotations

import os
import sys

import click
import psycopg

# ---------------------------------------------------------------------------
# Fixture data: ≥20 entities, ≥5 types, ≥10 relations, ≥3 observations each.
# All names are kebab-case. No absolute paths, no secrets, no junk content.
# ---------------------------------------------------------------------------

ENTITIES: list[dict] = [
    # --- pattern (5 entities) ---
    {
        "name": "jwt-auth-pattern",
        "entityType": "pattern",
        "observations": [
            "Use short-lived JWTs (15 min access token + 7-day refresh token) to limit blast radius of token theft.",
            "Store refresh tokens in HttpOnly Secure cookies, never in localStorage or sessionStorage.",
            "Include jti (JWT ID) claim and maintain a server-side blocklist for revocation before expiry.",
            "Sign with RS256 (asymmetric) for services that need to verify without the signing secret.",
        ],
    },
    {
        "name": "rate-limiting-pattern",
        "entityType": "pattern",
        "observations": [
            "Apply rate limits at the API gateway layer before requests reach service handlers.",
            "Use a sliding-window algorithm (Redis + token bucket) for per-IP and per-user-ID limits.",
            "Return 429 with Retry-After header; never expose internal queue depth in the response.",
            "Separate limits for read vs write endpoints — writes carry higher abuse risk.",
        ],
    },
    {
        "name": "idempotent-write-pattern",
        "entityType": "pattern",
        "observations": [
            "Idempotency keys (client-supplied UUID in header) let the client retry safely on network failure.",
            "Store the idempotency key and its response in Redis with a 24-hour TTL.",
            "ON CONFLICT DO NOTHING is the Postgres primitive for idempotent INSERT pipelines.",
            "Log deduplication hits separately from primary write metrics to detect client retry storms.",
        ],
    },
    {
        "name": "soft-delete-pattern",
        "entityType": "pattern",
        "observations": [
            "Set deleted_at = now() instead of DELETE to preserve audit history and enable recovery.",
            "Every read query filters WHERE deleted_at IS NULL; use partial indexes to keep scans fast.",
            "Expose a hard-delete admin endpoint only to service accounts — never to end users.",
            "Weekly pg_dump covers the 7-day recovery window for accidental soft-deletes.",
        ],
    },
    {
        "name": "vector-search-pattern",
        "entityType": "pattern",
        "observations": [
            "HNSW index with vector_cosine_ops gives sub-millisecond ANN search at ≤1 million vectors.",
            "Embed at query time with the same model used at insert time — model mismatch silently returns wrong results.",
            "Aggregate per-entity with MIN(distance) when observations are stored at row granularity.",
            "Use ef_search=64 for recall=0.95 trade-off; bump to 128 if precision degrades at scale.",
        ],
    },

    # --- decision (4 entities) ---
    {
        "name": "go-over-python-for-mcp-server",
        "entityType": "decision",
        "observations": [
            "Go static binary starts in 1-3 s on Render Free; Python equivalent is 15-30 s — kills free-tier UX.",
            "fastembed-go provides the same all-MiniLM-L6-v2 ONNX model as Python sentence-transformers.",
            "Trade-off: no code reuse with claude-dev-team knowledge-graph Python server; accepted as one-time cost.",
            "Revisit if the team needs Python-specific libraries that have no Go equivalent.",
        ],
    },
    {
        "name": "single-tenant-schema-v1",
        "entityType": "decision",
        "observations": [
            "No tenant_id column in v1 — single team, single shared KG, YAGNI for multi-tenancy.",
            "Adding tenant_id later is an additive migration: add column → backfill → switch queries → drop old default.",
            "RLS on Supabase Free pins access to the service-role key only — anon role gets no grants.",
            "Single-tenant keeps the query surface minimal and the indexes tight.",
        ],
    },
    {
        "name": "one-shot-migration-not-dual-write",
        "entityType": "decision",
        "observations": [
            "Dual-write doubles the failure surface (two backends, sync drift, partial-write reconciliation).",
            "Flag-day cutover is acceptable for a single-team deployment whose writes are idempotent.",
            "ChromaDB kept cold for 30 days post-cutover as a rollback path.",
            "import_to_supabase.py is the migration workhorse; import_from_chromadb.py is the UX wrapper.",
        ],
    },
    {
        "name": "free-tier-hosting-strategy",
        "entityType": "decision",
        "observations": [
            "Render Free + Supabase Free = $0/mo recurring; adequate for a single team's KG at low-MB data sizes.",
            "Weekly pg_dump replaces PITR (Supabase Free has no PITR); 7-day recovery window.",
            "Supabase auto-pause after 7 days of DB inactivity mitigated by 6-day SELECT 1 keepalive cron.",
            "If team SLA tightens, upgrading to paid Render tier is a one-click change.",
        ],
    },

    # --- stack-profile (4 entities) ---
    {
        "name": "context-harness-mcp",
        "entityType": "stack-profile",
        "observations": [
            "Go 1.23 + mcp-go (mark3labs) + pgx/v5 + pgvector-go + fastembed-go (ONNX all-MiniLM-L6-v2 384 dims).",
            "Deployed as a multi-stage Docker image on Render Free; DB is Supabase Free (pgvector).",
            "MCP transport: streamable-http (March 2025 spec revision); local dev uses stdio.",
            "Content Filter: three layers — syntactic (size + junk denylist), secrets (gitleaks + regex), taxonomy (enum + path rejection).",
        ],
    },
    {
        "name": "claude-dev-team-kg-stack",
        "entityType": "stack-profile",
        "observations": [
            "Python + FastMCP + ChromaDB (PersistentClient at ~/.claude/chromadb/).",
            "Embeddings computed by sentence-transformers all-MiniLM-L6-v2 (384 dims, same model as context-harness-mcp).",
            "Local-only; team sync via manual export.py → import.py JSON files in shared-knowledge/.",
            "Sunset planned after KG_BACKEND switch lands in claude-dev-team PR-9.",
        ],
    },
    {
        "name": "github-actions-ci-cd-stack",
        "entityType": "stack-profile",
        "observations": [
            "ci.yml: go build + go vet + staticcheck + go test (testcontainers, ubuntu-latest, Docker available).",
            "deploy.yml: goose up against Supabase then curl Render deploy hook on push to main.",
            "pg_dump_weekly.yml: Sunday 03:00 UTC encrypted dump (gpg --symmetric AES-256) retained 90 days.",
            "supabase_keepalive.yml: SELECT 1 every 6 days to prevent Supabase Free auto-pause.",
        ],
    },
    {
        "name": "pgvector-embedding-stack",
        "entityType": "stack-profile",
        "observations": [
            "pgvector extension on Postgres 16 — vector(384) column on observations table.",
            "HNSW index with vector_cosine_ops (m=16, ef_construction=64) on observations.embedding WHERE deleted_at IS NULL.",
            "pgvector-go v0.2.2 for type registration in pgxpool AfterConnect hook.",
            "Cosine distance query via <=> operator; aggregate per-entity with MIN(distance) in search_nodes.",
        ],
    },

    # --- service (4 entities) ---
    {
        "name": "render-mcp-service",
        "entityType": "service",
        "observations": [
            "Render Free web service; auto-deploys on push to main via deploy hook URL stored in GH secrets.",
            "Sleeps after 15 min of inactivity; Go binary cold starts in 1-3 s (non-embedding tools are sub-second).",
            "Healthcheck at /healthz: returns 200 once pgxpool SELECT 1 succeeds.",
            "region: oregon; dockerfilePath: Dockerfile; healthCheckPath: /healthz.",
        ],
    },
    {
        "name": "supabase-postgres-service",
        "entityType": "service",
        "observations": [
            "Supabase Free project: 500 MB DB ceiling, no PITR, auto-pauses after 7 days of DB inactivity.",
            "Connection string exposed as SUPABASE_DB_URL in GH secrets and Render env vars.",
            "RLS enabled: service-role key only; anon and authenticated roles have no grants.",
            "Weekly pg_dump artifact retained 90 days in GH Actions; restore via gpg --decrypt | psql.",
        ],
    },
    {
        "name": "mcp-server-healthz-endpoint",
        "entityType": "service",
        "observations": [
            "GET /healthz returns 200 {status:ok, db:ok} when pgxpool SELECT 1 succeeds.",
            "Returns 503 if DB is unreachable — used by Render health check and smoke scripts.",
            "Does not load the ONNX model; embedding health is inferred from the first successful search_nodes.",
            "Smoke test happy_path.sh calls /healthz first and aborts if it returns non-200.",
        ],
    },
    {
        "name": "claude-code-mcp-client",
        "entityType": "service",
        "observations": [
            "Claude Code reads ~/.claude.json for MCP server registration under mcpServers.memory.",
            "Local dev: type http, url http://localhost:8080/mcp.",
            "Production: type http, url https://<render-service>.onrender.com/mcp.",
            "KG_BACKEND env var (PR-9) will let the installer choose the URL automatically.",
        ],
    },

    # --- project (3 entities) ---
    {
        "name": "context-harness-mcp",
        "entityType": "project",
        "observations": [
            "Go MCP server replacing claude-dev-team's local ChromaDB KG with a shared Supabase backend.",
            "8 PRs: schema, validator, MCP tools, embeddings, Phase 1 deploy, Phase 2 deploy, migration scripts.",
            "Stage 2 closes with PR-8 (this migration tooling); PR-9 is the downstream claude-dev-team switch.",
            "GitHub: github.com/valianx/context-harness-mcp (public).",
        ],
    },
    {
        "name": "claude-dev-team",
        "entityType": "project",
        "observations": [
            "Distribution of Claude Code agent system: agents, skills, hooks, KG MCP server, cross-platform installer.",
            "Installer targets ~/.claude/ + ~/.claude.json; ChromaDB KG at ~/.claude/chromadb/.",
            "PR-9 will add KG_BACKEND=chromadb|supabase switch; default chromadb in v1.2.0, supabase in v1.3.0.",
            "After 30-day coexistence window, ChromaDB code is sunset in v2.0.0.",
        ],
    },
    {
        "name": "context-harness-mcp-backups",
        "entityType": "project",
        "observations": [
            "Private GitHub repo storing weekly encrypted pg_dump artifacts from the production Supabase.",
            "Artifacts retained 90 days via GH Actions retention policy.",
            "Restore: gpg --decrypt < dump.sql.gpg | psql $SUPABASE_DB_URL.",
            "DUMP_PASSPHRASE stored in GH secrets; rotate via openssl rand -base64 32.",
        ],
    },

    # --- constraint (2 entities, gives total ≥ 20) ---
    {
        "name": "embedding-model-locked-at-384-dims",
        "entityType": "constraint",
        "observations": [
            "all-MiniLM-L6-v2 produces 384-dim vectors; locked until ChromaDB migration is complete.",
            "Changing the model after cutover requires a full re-embed of the entire KG — a separate PR.",
            "Parity with ChromaDB's hnsw:space cosine is preserved; no semantic neighborhood shift on migration.",
            "Fixture JSONs must use 384-dim float32 arrays; import_to_supabase.py rejects any other dimensionality.",
        ],
    },
    {
        "name": "content-filter-three-layer-contract",
        "entityType": "constraint",
        "observations": [
            "Every write payload crosses three layers before INSERT: syntactic, secrets, taxonomy.",
            "Rejection at any layer aborts the entire call — no partial writes.",
            "Error codes are stable: policy/size-exceeded, policy/junk-pattern, policy/secret-detected, policy/taxonomy-violation.",
            "Error messages are in Spanish so Claude surfaces them to the user without re-translation.",
        ],
    },
]

# Relations: ≥10 crossing multiple entity types.
RELATIONS: list[dict] = [
    {"from": "context-harness-mcp", "to": "pgvector-embedding-stack", "relationType": "uses-stack"},
    {"from": "context-harness-mcp", "to": "github-actions-ci-cd-stack", "relationType": "uses-stack"},
    {"from": "context-harness-mcp", "to": "render-mcp-service", "relationType": "belongs-to"},
    {"from": "context-harness-mcp", "to": "supabase-postgres-service", "relationType": "depends-on"},
    {"from": "render-mcp-service", "to": "supabase-postgres-service", "relationType": "depends-on"},
    {"from": "claude-code-mcp-client", "to": "render-mcp-service", "relationType": "calls"},
    {"from": "claude-dev-team", "to": "claude-dev-team-kg-stack", "relationType": "uses-stack"},
    {"from": "context-harness-mcp", "to": "one-shot-migration-not-dual-write", "relationType": "relates_to"},
    {"from": "context-harness-mcp", "to": "go-over-python-for-mcp-server", "relationType": "relates_to"},
    {"from": "context-harness-mcp", "to": "free-tier-hosting-strategy", "relationType": "relates_to"},
    {"from": "context-harness-mcp", "to": "single-tenant-schema-v1", "relationType": "relates_to"},
    {"from": "vector-search-pattern", "to": "pgvector-embedding-stack", "relationType": "relates_to"},
    {"from": "jwt-auth-pattern", "to": "rate-limiting-pattern", "relationType": "relates_to"},
    {"from": "soft-delete-pattern", "to": "content-filter-three-layer-contract", "relationType": "relates_to"},
    {"from": "context-harness-mcp-backups", "to": "supabase-postgres-service", "relationType": "belongs-to"},
    {"from": "embedding-model-locked-at-384-dims", "to": "pgvector-embedding-stack", "relationType": "relates_to"},
    {"from": "idempotent-write-pattern", "to": "one-shot-migration-not-dual-write", "relationType": "relates_to"},
    {"from": "mcp-server-healthz-endpoint", "to": "render-mcp-service", "relationType": "belongs-to"},
    {"from": "context-harness-mcp", "to": "content-filter-three-layer-contract", "relationType": "depends-on"},
    {"from": "claude-dev-team", "to": "context-harness-mcp", "relationType": "depends-on"},
]


def truncate_tables(cur: psycopg.Cursor) -> None:
    """TRUNCATE all three tables (CASCADE handles FK ordering)."""
    cur.execute("TRUNCATE TABLE relations, observations, entities RESTART IDENTITY CASCADE")


def insert_entities(cur: psycopg.Cursor) -> int:
    """Insert all fixture entities; return count of rows inserted."""
    inserted = 0
    # project entities share a name with stack-profile — resolve uniqueness by type.
    # The DB enforces uniqueness on name alone, so 'context-harness-mcp' as both
    # project and stack-profile would conflict. Insert only unique names; the
    # stack-profile variant already has richer observations.
    seen_names: set[str] = set()
    for entity in ENTITIES:
        name = entity["name"]
        if name in seen_names:
            # Skip duplicate names gracefully (project vs stack-profile collision).
            continue
        seen_names.add(name)
        result = cur.execute(
            """
            INSERT INTO entities (name, entity_type)
            VALUES (%s, %s)
            ON CONFLICT (name) DO NOTHING
            RETURNING id
            """,
            (name, entity["entityType"]),
        )
        if result.fetchone():
            inserted += 1
    return inserted


def insert_observations(cur: psycopg.Cursor) -> int:
    """Insert observations for all entities; return count inserted."""
    inserted = 0
    seen_names: set[str] = set()
    for entity in ENTITIES:
        name = entity["name"]
        if name in seen_names:
            continue
        seen_names.add(name)

        row = cur.execute(
            "SELECT id FROM entities WHERE name = %s AND deleted_at IS NULL",
            (name,),
        ).fetchone()
        if row is None:
            continue
        entity_id = row[0]

        for text in entity["observations"]:
            result = cur.execute(
                """
                INSERT INTO observations (entity_id, text)
                VALUES (%s, %s)
                ON CONFLICT (entity_id, text) DO NOTHING
                RETURNING id
                """,
                (entity_id, text),
            )
            if result.fetchone():
                inserted += 1
    return inserted


def insert_relations(cur: psycopg.Cursor) -> int:
    """Insert fixture relations; return count inserted."""
    inserted = 0
    for rel in RELATIONS:
        from_row = cur.execute(
            "SELECT id FROM entities WHERE name = %s AND deleted_at IS NULL",
            (rel["from"],),
        ).fetchone()
        to_row = cur.execute(
            "SELECT id FROM entities WHERE name = %s AND deleted_at IS NULL",
            (rel["to"],),
        ).fetchone()
        if from_row is None or to_row is None:
            continue

        result = cur.execute(
            """
            INSERT INTO relations (from_entity_id, to_entity_id, relation_type)
            VALUES (%s, %s, %s)
            ON CONFLICT (from_entity_id, to_entity_id, relation_type) DO NOTHING
            RETURNING id
            """,
            (from_row[0], to_row[0], rel["relationType"]),
        )
        if result.fetchone():
            inserted += 1
    return inserted


def count_rows(cur: psycopg.Cursor) -> tuple[int, int, int]:
    """Return (entity_count, observation_count, relation_count) for active rows."""
    e = cur.execute("SELECT count(*) FROM entities WHERE deleted_at IS NULL").fetchone()[0]
    o = cur.execute("SELECT count(*) FROM observations WHERE deleted_at IS NULL").fetchone()[0]
    r = cur.execute("SELECT count(*) FROM relations WHERE deleted_at IS NULL").fetchone()[0]
    return e, o, r


@click.command()
@click.option(
    "--dsn",
    default=lambda: os.environ.get("SUPABASE_DB_URL", ""),
    show_default=True,
    help="Postgres DSN. Defaults to $SUPABASE_DB_URL.",
)
@click.option(
    "--reset",
    is_flag=True,
    default=False,
    help=(
        "TRUNCATE entities, observations, and relations before seeding. "
        "Use only against a local dev database — never against production."
    ),
)
def main(dsn: str, reset: bool) -> None:
    """Seed a local Postgres with deterministic KG fixtures (dev only)."""
    if not dsn:
        click.echo(
            "Error: --dsn is required or set SUPABASE_DB_URL.", err=True
        )
        sys.exit(1)

    try:
        with psycopg.connect(dsn) as conn:
            with conn.transaction():
                with conn.cursor() as cur:
                    if reset:
                        click.echo("--reset: truncating entities, observations, relations...")
                        truncate_tables(cur)

                    ent_inserted = insert_entities(cur)
                    obs_inserted = insert_observations(cur)
                    rel_inserted = insert_relations(cur)
                    ent_total, obs_total, rel_total = count_rows(cur)

    except Exception as exc:  # noqa: BLE001
        click.echo(f"Error during seed: {exc}", err=True)
        sys.exit(1)

    click.echo(
        f"seed complete: entities={ent_inserted} inserted, {ent_total} total | "
        f"observations={obs_inserted} inserted, {obs_total} total | "
        f"relations={rel_inserted} inserted, {rel_total} total"
    )


if __name__ == "__main__":
    main()
