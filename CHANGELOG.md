# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `feat(db): project scoping schema (00007)` — `migrations/00007_project_scope.sql`: adds `project_id text NOT NULL DEFAULT 'global'` to `nodes` and `relations`, drops legacy UNIQUE `entities_name_key`, adds composite UNIQUE `nodes_project_name_key (project_id, name)`, and three partial indexes (`nodes_project_name_active_idx`, `nodes_project_id_active_idx`, `relations_project_id_active_idx`). All existing rows backfill to `'global'` via PG16 fast-default path (O(1)).
- `feat(db): extend relation taxonomy with supersedes and conflicts_with (00008)` — `migrations/00008_taxonomy_extend.sql`: drops and recreates `relations_relation_type_check` with 7 values (`relates_to`, `belongs-to`, `calls`, `uses-stack`, `depends-on`, `supersedes`, `conflicts_with`). New types are descriptive-only — neither filters reads automatically.
- `feat(validate): projectid Check + DefaultProjectID` — `internal/validate/projectid.go`: exports `Check(name string) *Error` (regex `^[a-z][a-z0-9-]{0,63}$`) and `DefaultProjectID = "global"`. Called by write handlers before the Content Filter.
- `feat(validate): policy error codes for project scoping` — `internal/validate/errors.go`: adds `CodeProjectNamingViolation`, `CodeCrossProjectRelation`, `CodeNodeNotFound`, `CodeRelationAlreadyExists`, and `LayerProject`. Wire-stable strings — do not rename after PR-3 ships.
- `feat(validate): expand RelationTypes map` — `internal/validate/taxonomy.go`: adds `"supersedes"` and `"conflicts_with"` to `RelationTypes`, extending the closed enum from 5 to 7 values. `joinRelationTypes()` now returns 7 values ordered alphabetically.
- `ci(workflow): install ONNX Runtime on the CI runner` — `.github/workflows/ci.yml`: adds an `Install ONNX Runtime` step before `go test` that downloads `onnxruntime-linux-x64-1.20.0.tgz` from the official GitHub release, extracts it to `/usr/local`, runs `ldconfig`, and exports `LD_LIBRARY_PATH`. Without this, write-path tests (every test that triggers an embedding via `requireEmbedder()`) silently skipped in CI — the green check was misleading. Version pinned to match `Dockerfile` `ONNX_RUNTIME_VERSION` so CI exercises the same embedder path as production.

### Added (PR-3 of v0.4.0 — conflict detection tools)

- `feat(mcp): find_conflicts tool` — `internal/mcp/conflicts.go` + `internal/store/conflicts.go`: read-only tool that finds semantically similar nodes in the same project using the loop-N-queries strategy (one pgvector cosine query per target observation, client-side aggregation). Returns candidates ordered by similarity with matching observation pairs. Default `top_k=5`, max 50; default `min_similarity=0.85`. No rate-limit, no content filter.
- `feat(mcp): mark_superseded tool` — `internal/mcp/conflicts.go`: write tool that inserts a `supersedes(new → old)` relation in a single `pgx.Tx`. Enforces same-project invariant; rejects cross-project pairs with `policy/cross-project-relation`. Idempotent (`policy/relation-already-exists` on repeat). Optional `archive_old_observations: true` soft-deletes the old node's observations (`deleted_at`) — the only mechanism to hide old node content (reads do not filter by `supersedes` automatically). `reason` field is logged via `slog.Info` but NOT persisted in DB. Rate-limited (write tool).
- `docs(knowledge): v0.4.0 patterns and decisions` — `docs/knowledge.md`: 6 bullets covering project scoping, naming convention, taxonomy expansion, cross-project rejection, supersedes/conflicts_with descriptive-only semantics, and find_conflicts loop strategy. All tagged `v0.4.0`.
- `docs(mcp-tools): conflict detection section` — `docs/mcp-tools.md`: new "Conflict detection" section with full wire shapes, examples, error codes, and the prominently placed note that `supersedes`/`conflicts_with` are descriptive-only and `archive_old_observations` is the only escape hatch. New error codes (`policy/node-not-found`, `policy/cross-project-relation`, `policy/relation-already-exists`, `policy/project-naming-violation`) added to the policy errors table. Tool count updated from ten to twelve.

### Added (PR-2 of v0.4.0 — wiring)

- `feat(mcp): project field on write tools` — `create_nodes` and `create_relations` accept an optional `project` field (default `"global"`). Project names are validated server-side via `internal/validate/projectid.Check()` BEFORE the Content Filter — violations return `policy/project-naming-violation` with `layer: "project"`. `create_relations` rejects cross-project edges (different `project_id` on `from` vs `to`) with `policy/cross-project-relation`. `add_observations` and `update_observations` derive the project from the parent node — no `project` field on their input.
- `feat(mcp): project filter on read tools` — `search_nodes`, `open_nodes`, `read_graph`, `stats`, `timeline` accept an optional `project` filter. Omitting it returns ALL projects (back-compat). `stats` with a filter scopes counts, `by_type`, and `oldest_node`/`newest_node` to that project. `timeline` with a filter scopes the chronological list.
- `feat(store): extend signatures with projectID` — `Create`, `InsertRelation` gain `projectID string`; `FindByName`, `ListActive`, `SearchByCosine`, `ListByCreatedAt`, `Stats` gain `projectFilter *string` (nil → no filter; non-nil → SQL filter via `($N::text IS NULL OR project_id = $N)`).
- `feat(viewer): project dropdown + chip + /viewer/api/projects` — `<select id="project-filter" aria-label="filter by project">` rendered above the search box; first option `all projects` (value=""), then `global`, then others alphabetical. Populated at page load via `GET /viewer/api/projects` which returns `{"projects":[...]}`. Each node card gains a `.ch-badge-project` chip (WCAG AA contrast). Search URL includes `&project=` when not empty. `nodeView` JSON gains `project_id`.
- `feat(khctl): export --project flag` — restricts the dump to one project. Empty flag → all projects (default). Import already accepts `project_id` per node from PR-1; legacy exports without that field default to `'global'` and a structured `slog.Info` line surfaces the count of nodes that landed on the default so operators can detect a stale dump.
- `docs(mcp-tools): project scoping section` — adds a top-level "Project scoping" subsection documenting the model (default `'global'`, naming regex, default-allow on reads, same-project relations, NOT a tenant isolation mechanism). Taxonomy table updated from 5 to 7 relation types.

### Fixed

- `fix(khctl): import/export project_id round-trip` — `internal/khctl/import.go` and `internal/khctl/export.go` updated for the new `(project_id, name)` UNIQUE constraint added in migration 00007. Import: `ON CONFLICT ON CONSTRAINT nodes_project_name_key DO NOTHING`; both `ImportNode` and `ImportRelation` accept a `project_id` JSON field that defaults to `"global"` when absent (back-compat with pre-Phase-2 exports). Export: `project_id` is now emitted in both `ExportNode` and `ExportRelation` payloads. Internal node lookups (`importObservations`, `importRelations`) filter by `(project_id, name)` to avoid ambiguity across projects.



- `internal/web/landing.go` + `internal/web/static/landing.html` — new landing page served at `GET /`. Presents the two-product agent stack (`claude-dev-team` + `context-harness-mcp`) with equal billing: dual GitHub CTAs, dual nav links, install line pointing at `./bin/install.sh`. Uses the `agent-sphere` motif (wireframe sphere + amber hub + violet signal pulses across diametral chords). Same vanilla HTML/CSS/JS stack as the other static surfaces; no framework, no CDN deps. Registered last on the mux so it doesn't shadow `/mcp/`, `/auth/*`, `/healthz`, `/viewer/*`; non-`/` paths under `/` fall through to 404.
- `docs/email-templates/invite-email.html` + `docs/email-templates/recovery-email.html` — email templates for the operator to paste into the identity provider's email-template dashboard. Table-based layout, inline CSS only, `prefers-color-scheme: dark` media query, inline SVG mark (no remote images). Uses the IdP's `{{ .ConfirmationURL }}` / `{{ .Email }}` / `{{ .SiteURL }}` placeholders.
- `docs/design-system.md` — design spec covering palette, typography, the `agent-sphere` motif, and the 8 reusable components (`ch-mark`, `ch-btn-primary`/`-secondary`, `ch-input`, `ch-code`, `ch-card`, `ch-badge`, `ch-toast`, state classes). Reference for future surfaces.

### Changed

- `internal/web/static/callback.html` + `internal/web/static/login.html` — refactored to the `agent-sphere` design system (same template variables preserved: `{{.SupabaseProjectURL}}`, `{{.SupabaseAnonKey}}`, `{{.MCPPublicURL}}`). New visual identity, same behavior contract. Inline SVG mark + starfield background, amber accent + violet signals, JetBrains Mono for code snippets.
- `internal/viewer/templates/index.html` — full visual refactor to the agent-sphere design. JS rewired to call the existing `/viewer/api/search` endpoint (unauthenticated) instead of `/mcp/` directly; `/mcp/` requires auth when `MCP_AUTH=enabled` and the viewer is locked public read-only per the v0.2.0 architecture. Response shape mapping (`node_type` → `nodeType`, `relations_out`/`relations_in` → unified `relations`) lives in `searchAPI()`.
- `tests/viewer_test.go` `TestViewerIndex` — assertion updated to match the new wordmark ("context-harness" lowercase + "knowledge graph") since the old "Context Harness MCP" title was retired in the design refactor.
- `cmd/server/main.go` — registers `web.RegisterLanding(mux)` for `GET /` after the other unauthenticated routes.

### Changed

- **SOFT-BREAKING: `GET /healthz` HTTP behavior change** (`cmd/server/main.go` + `internal/healthz/healthz.go`) — the endpoint now returns **HTTP 200** when all checks pass and **HTTP 503** when any check fails (degraded), replacing the previous always-200 response. The body shape changes from `{"status":"ok","db":"not-configured"}` to `{"checks":[...],"degraded":bool}`, matching the new `doctor` MCP tool. Container hosts (Railway, Render, Fly, Docker compose) automatically benefit from the 503 signal to mark the container unhealthy. Operators who currently rely on the always-200 response or parse the `status`/`db` fields must update their healthcheck to use the HTTP status code and the new body shape.

- **Platform-agnostic positioning refactor**: removed all hosting-provider-specific assumptions from code, config, docs, and KG fixtures. Render is no longer a privileged target — the product is positioned as deployable to any container host (Railway / Render / Fly / Coolify / self-hosted Docker / etc.), with `docker compose up` for local mode as a first-class peer to cloud deployments. ChromaDB / `claude-dev-team`-as-source positioning also removed — `context-harness-mcp` is the standalone memory MCP, not a "replacement for". Concrete changes:
  - `render.yaml` deleted (no longer needed; operators configure their own platform).
  - `.github/workflows/deploy.yml` rewritten: `RENDER_DEPLOY_HOOK_URL` → generic `DEPLOY_HOOK_URL` (with backward-compat fallback for one release); deploy hook step skips with a notice when unset (platforms that auto-deploy from git push don't need it).
  - `docs/deployment.md` rewritten from a Render-specific runbook to a platform-agnostic guide (principles + 5 platform examples at equal weight).
  - `docs/cutover-playbook.md` deleted (one-time ChromaDB→Postgres migration runbook, obsolete now that the migration is done).
  - `docs/knowledge.md` appended with `[decisión]` and `[stack]` bullets that supersede the original Render/ChromaDB-locked entries (existing bullets preserved per append-only convention).
  - `CLAUDE.md` §1/§3/§4 rewritten: "Render Free" → "container hosting (any provider)"; Phase 2 framed as "remote container hosting (operator's choice)".
  - `README.md` install section repositioned: "Cloud (Render Free + Supabase Free)" → "Cloud (any container hosting + any Postgres+pgvector)". Tagline updated.
  - `docs/auth.md`, `docs/mcp-tools.md`, `docs/local-stack.md`, `docs/roadmap.md` edited to platform-agnostic prose.
  - `internal/khctl/seed.go` KG fixtures rewritten: removed `claude-dev-team-kg-stack` and `one-shot-migration-not-dual-write` nodes (obsolete); renamed `render-mcp-service` → `mcp-server-deployment`, `supabase-postgres-service` → `postgres-pgvector-service`, `free-tier-hosting-strategy` → `containerized-deployment-strategy`, `go-over-python-for-mcp-server` → `go-for-mcp-server-runtime`; added new `two-deployment-modes-stack` and `auth-opt-in-via-mcp-auth-env` nodes positioning local + cloud as first-class equals. Relations updated accordingly.
  - Code comments neutralized in `cmd/server/main.go`, `cmd/server/main_test.go`, `internal/ratelimit/limiter.go`.
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

- `internal/store/observations.go` + `internal/mcp/nodes.go` — `update_observations` tool: atomically replaces an existing observation on a node via soft-delete of old text + insert of new text with fresh embedding, all within a single `pgx.Tx`. Runs the full Content Filter (size, secrets, taxonomy) on `new_text` before opening any transaction; `old_text` is a lookup-only key. Rejects `old_text == new_text` before opening Tx (AC-5). Attribution (`created_by_user_id`/`created_by_email`) persisted on the new observation row. Error on node-not-found or observation-not-found triggers full rollback (AC-3, AC-4). Rate-limited (write tool, same bucket as `add_observations`).

- `internal/healthz/healthz.go` + `internal/mcp/server.go` + `internal/validate/secrets.go` — `doctor` MCP tool: runs 5 deep operational probes in order (`db_ping`, `pgvector_extension`, `embedder`, `gitleaks_detector`, `row_counts`), each with a 5 s per-check timeout. Always returns `IsError:false` — degradation is reported in the body via `degraded:true` and per-check `status:"fail"`. Read-only, no rate-limit, no content filter.

- `internal/mcp/query.go` + `internal/store/nodes.go` + `migrations/00006_nodes_created_at_idx.sql` — `timeline` tool: chronological node listing with optional RFC3339 `since`/`until` date bounds, offset-based pagination (`limit` default 50 max 200, `offset` default 0 max 100000), stable `ORDER BY created_at DESC, id DESC`, `has_more` flag via LIMIT N+1 strategy, and relations scoped to the result set. Read-only, no rate-limit, no content filter.

- `internal/mcp/query.go` + `internal/store/nodes.go` — `stats` tool: server-side aggregated counts (`node_count`, `observation_count`, `relation_count`, `by_type`, `oldest_node`/`newest_node`) replacing client-side `read_graph` counting. Read-only, no rate-limit, no content filter.

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
