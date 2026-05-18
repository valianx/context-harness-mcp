# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `render.yaml` — fully spec'd for Phase 2 Render deploy: `region: oregon`, `dockerfilePath`, `dockerContext`, static env vars (`MCP_TRANSPORT`, `MCP_HTTP_ADDR`, `LOG_LEVEL`), and `SUPABASE_DB_URL` with `sync: false` (must be set manually in Render dashboard; never sourced from git).
- `.github/workflows/deploy.yml` — push-to-main CD: runs `goose up` against Supabase (migrations before code), then curls the Render deploy hook. Guard step exits 0 cleanly when secrets are unset. `concurrency: group: deploy-main` prevents simultaneous deploys.
- `.github/workflows/pg_dump_weekly.yml` — Sunday 03:00 UTC encrypted backup: `pg_dump --no-owner --no-privileges`, AES-256 via `gpg --symmetric`, uploaded as GH Actions artifact with 90-day retention.
- `.github/workflows/supabase_keepalive.yml` — `SELECT 1` every 6 days at 12:00 UTC to prevent Supabase Free auto-pause (7-day inactivity threshold, 1-day safety buffer).
- README `§Deployment` — one-time setup runbook (Supabase, Render, deploy hook, passphrase, GH secrets), GH secrets reference table, deploy verification steps, Claude Code `~/.claude.json` snippet, free-tier op model, and recovery-from-backup procedure.

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
