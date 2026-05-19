# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- BREAKING: env var `SUPABASE_DB_URL` renamed to `DATABASE_URL` (industry standard; the server works with any Postgres+pgvector, not specifically Supabase). The server reads `DATABASE_URL` primarily and falls back to `SUPABASE_DB_URL` with a stderr deprecation warning for one release. Operators should rename their secret in their hosting platform (Railway, Render, etc.) and any local `.env` file.
- `internal/config/dbenv.go` — new shared helper `ResolveDatabaseURL()` used by both `cmd/server` and `cmd/khctl`; reads `DATABASE_URL` with fallback to deprecated `SUPABASE_DB_URL`.
- `cmd/server/main.go`, `cmd/khctl/dsn.go` — updated to use `config.ResolveDatabaseURL()`; error messages reference `DATABASE_URL`.
- `docker-compose.yml` — env var pass-through updated to `DATABASE_URL` with backward-compat fallback to `SUPABASE_DB_URL` for one release.
- `render.yaml` — `SUPABASE_DB_URL` env var key renamed to `DATABASE_URL`.
- `.env.example` — `SUPABASE_DB_URL` renamed to `DATABASE_URL`; comment block updated to explain provider-agnostic scope and backward-compat.
- `.github/workflows/deploy.yml`, `pg_dump_weekly.yml`, `supabase_keepalive.yml` — secrets fallback expression `${{ secrets.DATABASE_URL != '' && secrets.DATABASE_URL || secrets.SUPABASE_DB_URL }}` so existing GH secrets keep working until operators rename them.
- `.github/workflows/ci.yml` — `SUPABASE_DB_URL` test env renamed to `DATABASE_URL`.
- `docs/deployment.md`, `docs/local-stack.md`, `docs/cutover-playbook.md`, `docs/knowledge.md` — all references updated; backward-compat notes and migration guidance added.
- `README.md`, `CLAUDE.md` §1/§3/§4 — `SUPABASE_DB_URL` references renamed.

### Deprecated

- `SUPABASE_DB_URL` env var. Accepted as fallback with stderr warning for one release; will be removed in v2.0. Use `DATABASE_URL` instead.

### Added

- `docs/auth.md` (nuevo, 5 secciones) + ADR en `docs/knowledge.md` + actualización de `CLAUDE.md` §3/§4/§5 — runbook completo de auth: admin setup (Supabase + Database Webhook con bug #38848 gotcha), dev flow walkthrough, revocation paths (primary webhook + fallback cron), nuclear secret rotation procedure, y FAQ con troubleshooting de cada error code. Cierre de docs para v0.2.0 Phase 0 Auth/Security.

- `internal/mcp/{nodes,relations}.go` + `internal/store/{nodes,observations,relations}.go` — wire de atribución: `execCreate*` y `execAdd*` leen `auth.UserIDFromContext(ctx)` + `auth.EmailFromContext(ctx)` y los persisten en las columnas `created_by_user_id` + `created_by_email` agregadas por PR-1. Cuando el ctx no tiene user (stdio o `MCP_AUTH=none`), persiste NULL — comportamiento idéntico a pre-PR-5. SQL queda parameterized (no `fmt.Sprintf` en strings SQL).
- `internal/web/webhook.go` + `internal/khctl/sync.go` + `cmd/khctl/sync_users.go` + `.github/workflows/users_sync.yml` — revocación instantánea vía Supabase Database Webhook sobre `auth.users` (HMAC-verified con `hmac.Equal` constant-time, body parsing tolerante a fields desconocidos) + fallback de reconciliación cada 6h vía GitHub Action que corre `khctl sync-users` contra Supabase Admin API (4 reglas: revoked-only, ban-applied, unban, deleted). Webhook actualiza `users.revoked_at` y dispara `revocationCache.Invalidate(sub)` para latencia E2E ≤2s. Counters `expvar` (`auth_webhook_{received,accepted,rejected}_total`) exponen métricas via `/debug/vars` cuando `MCP_EXPOSE_EXPVAR=1`. Env vars nuevas: `SUPABASE_WEBHOOK_SECRET`, `SUPABASE_SERVICE_ROLE_KEY`, `MCP_EXPOSE_EXPVAR`.
- `internal/web/` (callback, login, auth_exchange handlers + 2 static HTMLs vía `go:embed`) + `internal/auth/supabase_client.go` (wrapper de `GET /auth/v1/user`) + `internal/store/users.go` (Upsert, GetByID, SetRevoked, Delete) — endpoints HTTP unautenticados `/auth/callback`, `/auth/exchange`, `/auth/login` para el flow de magic link. `/auth/exchange` valida access_token contra Supabase (algorithm-agnostic via `GET /auth/v1/user`), upserta la row en `users` y emite el MCP JWT, todo en una transacción atómica con ROLLBACK si `IssueMCPToken` falla. Response 200 incluye `{token, expires_at, snippet}` listo para pegar en `~/.claude.json`. Sin `Set-Cookie` (viewer queda público read-only). Env vars nuevas: `SUPABASE_PROJECT_URL`, `SUPABASE_ANON_KEY`, `MCP_SNIPPET_SERVER_NAME` (default `"context-harness"`).

- `internal/auth/` (6 archivos) + `internal/ratelimit/{subkey,stdio}.go` (2 archivos) + middleware wired en `cmd/server/main.go` — Phase 0 Auth/Security middleware: JWT HS256 con expiry 1 año, `MCP_AUTH=none|enabled` env (default `none` para back-compat con consumidores actuales), revocation cache TTL 1h con `Invalidate(sub)` para cache-aside post-webhook, error codes estructurados (`auth/unauthenticated`, `auth/invalid-token`, `auth/expired`, `auth/revoked`, `auth/webhook-invalid-signature`) con shape JSON `{code, message, auth_login_url, layer:"auth"}` y mensajes en español. Rate limit ahora por `sub` claim (HTTP authed), fallback a IP (HTTP no-auth), y bucket process-wide para stdio (`MCP_STDIO_RATE_LIMIT=1000` burst, refill 100/s). Algorithm-confusion attack prevention vía `WithValidMethods(["HS256"])`. Nuclear rotation del `MCP_JWT_SECRET` (no two-slot, runbook en docs/auth.md de PR-6).
- `migrations/00004_auth_users.sql` + `migrations/00005_attribution.sql` — nueva tabla `users` (rastrea identidad de usuarios Supabase, `revoked_at` para revocación) y columnas UUID `created_by_user_id` + text `created_by_email` nullable en `nodes`/`observations`/`relations` (atribución; FK con `ON DELETE SET NULL` preserva el audit trail cuando se elimina un usuario). Índices parciales para el hot path de usuarios activos y lookups de atribución por tabla.

- `docs/roadmap.md` — v0.2.0 roadmap covering team-features evolution: Supabase Auth + JWT-based bearer with webhook-driven revocation (Phase 0), `update_observations`/`stats`/`timeline`/`doctor` tools (Phase 1), optional `project_id` scoping (Phase 2), semantic conflict detection via pgvector with `supersedes`/`conflicts_with` relations (Phase 3), sessions + passive capture hooks (Phase 4), and operational polish (Phase 5). Framed as open-source product deployable as single-tenant private instance.
- `cmd/khctl/` — Go binary (`khctl`) with three subcommands (`seed`, `export`, `import`) replacing the deleted Python migration scripts. Built with `CGO_ENABLED=0` (portable static binary, no ONNX dependency). `seed` populates ≥20 fixture nodes across ≥5 types with ≥3 observations each; `export` dumps active KG content to JSON; `import` loads JSON and accepts both `{"nodes":[...]}` and legacy `{"entities":[...]}` shapes. Baked into the Docker image at `/usr/local/bin/khctl`.
- `internal/khctl/` — exported package containing the core logic for seed, export, and import operations, enabling direct function calls from integration tests without `os/exec`.
- `cmd/khctl/import_test.go`, `cmd/khctl/export_test.go`, `cmd/khctl/seed_test.go` — integration tests for all three subcommands using testcontainers-go (`pgvector/pgvector:pg16`); cover idempotency, round-trip fidelity, legacy shape acceptance, embedding validation, and minimum-count guarantees.

### Changed

- Default HTTP port changed from `:8080` to `:7654` across `cmd/server/main.go`, `Dockerfile`, `docker-compose.yml`, `.env.example`, `render.yaml`, `.github/workflows/ci.yml`, all smoke scripts, and all documentation.
- `Dockerfile` — added `khctl` binary build (`CGO_ENABLED=0`) in the builder stage and `COPY` into the runtime image; updated `EXPOSE` from `8080` to `7654`.
- `tests/migration_test.go` — rewritten to call `internal/khctl` Go functions directly (`ParseImportPayload`, `RunImport`, `BuildExportPayload`) instead of shelling out to `uv run scripts/*.py`; removes the `uv` runtime dependency from the test suite.
- `docs/cutover-playbook.md` — replaced all `uv run scripts/import_to_supabase.py` / `uv run scripts/export_from_supabase.py` references with `docker compose exec mcp khctl import` / `khctl export`; pre-flight checklist updated to verify `khctl` availability instead of `uv`.
- `README.md` — Tech stack entry updated: "Operator scripts in Python via uv" → "Operator tooling (khctl) is also Go — no Python/uv runtime required."

### Removed

- `scripts/import_to_supabase.py`, `scripts/export_from_supabase.py`, `scripts/seed_dev.py`, `scripts/import_from_chromadb.py` — superseded by `cmd/khctl/`.
- `scripts/pyproject.toml`, `scripts/uv.lock` — Python scripts project removed with the scripts.

- `internal/viewer/`: public single-page web viewer at `/viewer/` (HTML + JS embedded via go:embed). Search box with semantic-search via embed.Default + pgvector cosine. Default view lists all active nodes. Same exposure level as the MCP read tools — public/unauthenticated, served on port 8080 alongside `/mcp` and `/healthz`.

### Changed

- Renamed all `entity`/`entities` vocabulary to `node`/`nodes` across the codebase (PR-3): SQL columns `entity_type`→`node_type`, `entity_id`→`node_id`, `from_entity_id`→`from_node_id`, `to_entity_id`→`to_node_id`; Go types `EntityRow`→`NodeRow`, `KindEntities`→`KindNodes`, `RejectedEntityIndex`→`RejectedNodeIndex`; JSON keys `created_entities`→`created_nodes`, `entity_count`→`node_count`, `rejected_entity_index`→`rejected_node_index`; files `internal/store/entities.go`→`nodes.go`, `internal/mcp/entities.go`→`nodes.go`. The 9 `node_type` enum values and the `relations` schema are unchanged.
- `migrations/00003_rename_entities_to_nodes.sql` — renames `entities` table to `nodes`, all affected columns and indexes, and `v_active_entities` view to `v_active_nodes`.
- `docs/mcp-tools.md` — rewritten for 6-tool surface; removed `delete_entities`, `delete_observations`, `delete_relations` tool docs; added `§Administrative deletions` section with SQL snippets for operator use.
- `scripts/seed_dev.py`, `scripts/export_from_supabase.py`, `scripts/import_to_supabase.py` — updated to use `nodes` table and `node_type`/`node_id` columns. Import script defensively accepts both `{"nodes": [...]}` and legacy `{"entities": [...]}` shapes with a deprecation warning.
- `scripts/smoke/happy_path.sh` — updated to use `create_nodes`/`nodeType`; removed `delete_entities` cleanup step (delete tools no longer in MCP surface).
- `scripts/smoke/secret_rejected.sh`, `scripts/smoke/size_rejected.sh`, `scripts/smoke/ratelimit_test.sh` — updated to use `create_nodes`/`nodeType`/`nodeName`.

### Removed

- `delete_entities`, `delete_observations`, `delete_relations` MCP tools removed from the public server surface (PR-3). Exposing destructive ops on an unauthenticated endpoint creates an uncontrolled mass-delete vector. Soft-delete functions remain at the store layer for admin-script use only.

### Added

- `SECRET_MODE=reject|redact` env var: opt-in redact mode replaces matched secret spans with `[REDACTED]` in-place and lets the call proceed; default `reject` preserves existing behavior. Fail-fast on unknown values. (`internal/validate/mode.go`, `internal/validate/secrets.go`, `cmd/server/main.go`)
- Per-IP write-tool rate limit: 10 writes per 10 seconds, token bucket, applied to `create_entities`, `add_observations`, `create_relations`. Reads and deletes unconstrained. Client IP from `X-Forwarded-For` (Render) or `RemoteAddr`. New `policy/rate-limited` error code. (`internal/ratelimit/`, `internal/validate/errors.go`)
- `internal/validate/secrets_test.go` — unit tests for redact mode: `TestRedactMode_ReplacesAWSKey`, `TestRedactMode_MultipleSecrets`, `TestRedactMode_PreservesContextAroundMatch`, `TestRejectMode_StillWorks`.
- `internal/ratelimit/limiter_test.go` — unit tests for per-IP rate limiter: first-10-pass, 11th-rejected, independent-IPs, recovers-over-time.

### Changed

- `validate.Run` signature changed from `(p Payload, k Kind)` to `(p *Payload, k Kind)` to enable in-place mutation for redact mode. All 5 call sites updated.
- `docs/mcp-tools.md` — added size limits section (5000 chars/obs, 50 KB/call), secret detection modes section, rate limit section, and updated policy error code table.
- `docs/deployment.md` — added `SECRET_MODE` configuration note and rate limit / `X-Forwarded-For` note.
- `docs/knowledge.md` — added `[restricción]` bullets for per-IP rate limit and `SECRET_MODE`.
- `.env.example` — documented `SECRET_MODE` with trade-off explanation.
- `go.mod` — added `golang.org/x/time v0.5.0` (direct dep for `rate.Limiter`).

- `docs/deployment.md` — Phase 2 cloud-deployment runbook extracted from the README: architecture overview, one-time Supabase + Render + GH-secrets setup, on-each-push behaviour, secrets reference, deploy verification, free-tier op model, recovery from backup.
- `docs/mcp-tools.md` — reference for the 9 MCP tools + healthz: arguments, success responses, taxonomy enums, atomicity guarantees, policy error codes with full structured-error shape.
- `scripts/pyproject.toml` — uv-managed scripts project (psycopg[binary], pgvector, chromadb, click); `package = false` so scripts run via `uv run` without installation.
- `scripts/import_to_supabase.py` — generic idempotent JSON → Supabase importer (claude-dev-team export.py shape); ON CONFLICT DO NOTHING for entities, observations, and relations; embedding passthrough with 384-dim validation.
- `scripts/import_from_chromadb.py` — convenience wrapper that auto-locates claude-dev-team's `export.py` (or accepts `--source-export PATH`) and pipes through `import_to_supabase.py`.
- `scripts/export_from_supabase.py` — inverse of `import_to_supabase.py`; exports active KG content to JSON in the same shape as claude-dev-team's `export.py`.
- `scripts/seed_dev.py` — deterministic KG fixtures (≥20 entities across ≥5 entity types, ≥20 relations); idempotent ON CONFLICT DO NOTHING; `--reset` flag for clean-slate seeding; dev-only, not for production.
- `tests/migration_test.go` — `TestMigrationRoundTrip`: seeds testcontainer Postgres via `import_to_supabase.py`, exports via `export_from_supabase.py`, diffs entities / observations / relations and embedding round-trip within ε=1e-6; skips gracefully when Docker or `uv` is unavailable.
- `tests/fixtures/migration_input.json` — synthetic parity fixture: 5 entities across 3 types (pattern, stack-profile, decision), 3 observations each, 384-dim zero-vector embeddings for round-trip fidelity testing.
- `docs/cutover-playbook.md` — operator runbook for the ChromaDB → Supabase flag-day cutover: §1 pre-flight checklist, §2 six numbered flag-day steps, §3 rollback criteria with explicit thresholds, §4 rollback procedure, §5 GH-Actions secret rotation runbook (SUPABASE_DB_URL / DUMP_PASSPHRASE / RENDER_DEPLOY_HOOK_URL), §6 Phase 1 local docker-compose emergency fallback.

### Changed

- `docs/knowledge.md` — added `[patrón]` bullet: cutover playbook lives in `docs/cutover-playbook.md` (committed, not session-docs) so operators on flag day have it locally.

- `render.yaml` — fully spec'd for Phase 2 Render deploy: `region: oregon`, `dockerfilePath`, `dockerContext`, static env vars (`MCP_TRANSPORT`, `MCP_HTTP_ADDR`, `LOG_LEVEL`), and `SUPABASE_DB_URL` with `sync: false` (must be set manually in Render dashboard; never sourced from git).
- `.github/workflows/deploy.yml` — push-to-main CD: runs `goose up` against Supabase (migrations before code), then curls the Render deploy hook. Guard step exits 0 cleanly when secrets are unset. `concurrency: group: deploy-main` prevents simultaneous deploys.
- `.github/workflows/pg_dump_weekly.yml` — Sunday 03:00 UTC encrypted backup: `pg_dump --no-owner --no-privileges`, AES-256 via `gpg --symmetric`, uploaded as GH Actions artifact with 90-day retention.
- `.github/workflows/supabase_keepalive.yml` — `SELECT 1` every 6 days at 12:00 UTC to prevent Supabase Free auto-pause (7-day inactivity threshold, 1-day safety buffer).
- README `§Deployment` — one-time setup runbook (Supabase, Render, deploy hook, passphrase, GH secrets), GH secrets reference table, deploy verification steps, Claude Code `~/.claude.json` snippet, free-tier op model, and recovery-from-backup procedure.
- `README.md` rewritten as a simple landing page: title "Context Harness MCP", functional description, two install paths (local Docker / cloud), documentation index. Deployment runbook moved to `docs/deployment.md`; tool reference extracted into `docs/mcp-tools.md`.

### Changed

- `README.md` — status line updated from "Stage 1 — no implementation yet" to "Phase 1 + Phase 2 live (as of PR-7)"; `§Quickstart` added pointing at `docs/local-stack.md`.
- `docs/knowledge.md` — `[restricción]` GH secrets bullet updated: `SUPABASE_ACCESS_TOKEN` flagged as optional/future-use (no current workflow consumes it); guard-step exit-0 behavior documented.

- `scripts/smoke/happy_path.sh` — end-to-end smoke test: `create_entities` → `read_graph` round-trip → `delete_entities` cleanup; exits 0 on success.
- `scripts/smoke/secret_rejected.sh` — smoke test asserting that an AWS-shaped credential in an observation returns `code="policy/secret-detected"` from the Content Filter.
- `scripts/smoke/size_rejected.sh` — smoke test asserting that a >65 KB observation returns `code="policy/size-exceeded"` from the syntactic layer.
- `docs/local-stack.md` — contributor runbook: quick start, DB target options (Supabase Free + local pgvector), migration invocation, smoke test execution, Claude Code `~/.claude.json` wiring, emergency fallback procedure, and troubleshooting table.
- `e2e-smoke` CI job in `.github/workflows/ci.yml` — boots `docker-compose.yml` against a `pgvector/pgvector:pg16` service container, applies migrations via goose, waits for the `mcp` healthcheck, then runs the three smoke scripts to completion.
- Healthcheck on the `mcp` service in `docker-compose.yml`: `curl -fsSL http://localhost:8080/healthz`, interval 10s, timeout 3s, retries 5, start_period 15s.
- `COPY --from=builder /src/migrations /migrations` in `Dockerfile` runtime stage — migrations are now baked into the image at `/migrations` so the `migrate` compose profile needs no host bind-mount.
- `all-MiniLM-L6-v2` ONNX model baked into the image at `/local_cache/fast-all-MiniLM-L6-v2/` via `sentence-transformers-all-MiniLM-L6-v2.tar.gz` (GCS public archive) — eliminates runtime model download and makes cold starts deterministic. `onnxruntime.so` symlink added for `dlopen` compatibility. `model_optimized.onnx` symlink bridges fastembed-go v1.0.0's hardcoded filename vs the archive's `model.onnx`.
- Three new bullets in `docs/knowledge.md`: `[patrón]` phased deploy, `[restricción]` same-image-both-phases, `[stack]` goose as migration tool.

### Changed

- `.github/workflows/ci.yml` — `lint-and-build` job extended with `go test ./...`; new parallel `e2e-smoke` job added.
- `Dockerfile` — runtime stage now includes: `COPY --from=builder /src/migrations /migrations`; `onnxruntime.so` symlink; baked ONNX model; goose-builder rebuilt with `-ldflags="-s -w"` and postgres-only tags to reduce binary from 52 MB to 13 MB.
- `docker-compose.yml` — `mcp` service now has a `healthcheck`; `migrate` service comment updated to reflect that migrations are baked into the image.

- `migrations/00001_init.sql` — goose-annotated migration: pgvector extension, `entities` / `observations` / `relations` tables with soft-delete columns, HNSW cosine index on `observations.embedding`, CHECK constraints on `entity_type` (9 values) and `relation_type` (5 values), and unique constraints for dedup.
- `migrations/00002_soft_delete.sql` — goose-annotated migration: partial indexes on `deleted_at IS NULL` for active-row query paths; `v_active_entities` restore helper view.
- `migrations/README.md` — documents the two goose application paths (docker compose `migrate` profile for manual dev, `deploy.yml` for CI/CD — both target Supabase), the testcontainers test path, and why `supabase db push` is not used.
- `internal/store/pool.go` — `New(ctx, dsn)` returns a `*pgxpool.Pool` with `pgxvec.RegisterTypes` in `AfterConnect`, `MaxConns=10`, and sensible timeouts.
- `tests/setup_test.go` — `TestMain` using testcontainers-go `pgvector/pgvector:pg16` ephemeral container + `goose.Up` library for migration application; `NewTestPool(t)` and `CleanDB(t)` helpers for downstream tests; graceful `t.Skip` when Docker daemon is unavailable.
- `tests/schema_test.go` — integration tests verifying the vector extension, all expected columns (including `deleted_at`), CHECK constraints for all 9 entity types and 5 relation types, unique constraints, and soft-delete view behavior.
- Go 1.23 module (`github.com/mariogutierrez/context-harness-mcp`) with direct
  dependencies declared: `mcp-go`, `pgx/v5`, `pgvector-go`, `validator/v10`, `goose/v3`.
- `cmd/server/main.go` — entry point with `-transport=stdio|http` and `-addr` flags,
  `log/slog` JSON structured logging.
- `internal/mcp/server.go` — `mcp-go` server factory; `RegisterHealthz` scaffold ready
  for PR-3/PR-4 to extend.
- `internal/healthz/healthz.go` — `healthz` MCP tool handler returning
  `{"status":"ok","db":"not-configured"}` (DB wiring lands in PR-2).
- `Dockerfile` — multi-stage build: `golang:1.23` server builder, `golang:1.23` goose
  builder, `debian:bookworm-slim` runtime with ONNX shared library at `/usr/local/lib`
  and both `server` + `goose` at `/usr/local/bin`.
- `docker-compose.yml` — Phase 1 stub declaring `postgres`, `migrate`, `mcp` services
  with `depends_on` ordering (full wiring in PR-6).
- `render.yaml` — Render IaC stub: `env: docker`, `plan: free`, `healthCheckPath: /healthz`
  (full env vars and region in PR-7).
- `.env.example` — documented env vars with placeholder values; no secrets.
- `CLAUDE.md` — repo conventions: §1 Purpose, §2 Repo Map, §3 Tech Stack,
  §4 Golden Commands, §5 Architectural Conventions, §6 Mandatory Working Agreements.
- `docs/knowledge.md` — seeded with `[stack]`, `[decisión]`, `[restricción]`, and
  `[patrón]` bullets from the design phase.
- `.github/workflows/ci.yml` — CI stub on `pull_request`: setup-go, `go vet ./...`,
  `staticcheck ./...`, `go build ./...`.

### Changed

- `docker-compose.yml` — Phase 1 stack simplified to run only the `mcp` service against
  Supabase via `SUPABASE_DB_URL`. Local Postgres + pgvector service removed. Migrations
  move to an opt-in `migrate` profile (`docker compose --profile migrate run --rm migrate`).
  Test strategy switches to `testcontainers-go` for ephemeral pg+pgvector per test run,
  removing the integration-test dependency on docker-compose. Phase 1's "operational
  fallback" role is dropped; the other three (E2E deploy validation, dev environment,
  self-hosting base) are preserved.
- `.env.example` — `POSTGRES_DSN` replaced by `SUPABASE_DB_URL` (required env var,
  no default in compose via `:?` syntax). Same DB target for Phase 1 and Phase 2.
- `CLAUDE.md` §1 Hosting, §2 Repo Map, §3 Tech Stack, §4 Golden Commands, §5
  Architectural Conventions updated to reflect the single Supabase target.
- `docs/knowledge.md` — bullets updated to reflect single Supabase target + testcontainers
  test strategy + three-role Phase 1.

### Fixed

- Removed hardcoded `POSTGRES_PASSWORD=postgres` from `docker-compose.yml` flagged by
  GitGuardian (incident #22875055). The value was a public-knowledge dev default with
  zero real-world credential value, but the pattern matched secret-detection rules.
  Replaced with the `SUPABASE_DB_URL` env var (no defaults, fails fast if unset).
