# CLAUDE.md — context-harness-mcp

> Bootstrap config for Claude Code in this repository. Keep it actionable.
>
> **Design rationale:** see `session-docs/initial-design/01-architecture.md`.

---

## 1. Purpose & Boundaries

**What this repo is.** `context-harness-mcp` is a **Go MCP server** that exposes a
6-tool Knowledge-Graph surface (`create_nodes`, `add_observations`, `create_relations`,
`search_nodes`, `open_nodes`, `read_graph`) backed by **Postgres + pgvector** (Supabase
in production) and deployable as a static Docker binary to **Render Free**. Delete
operations are intentionally not exposed as MCP tools — only via store-level SQL for
operators (see `docs/mcp-tools.md §Administrative deletions`).

**Primary audience:** agents and developers who work in the `context-harness-mcp`
repository. The orchestrator pipeline reads this file before touching any code.

**What this repo is NOT.**
- Not a general-purpose MCP framework — it encodes one specific KG schema.
- Not a Python service — the server is Go only. Operator tooling (`khctl`) is also Go.
- Not multi-tenant in v1 — single schema, no `tenant_id`.

**External dependencies (required).**
- `go` 1.23+ — runtime and build. Install via `mise install go` or from https://go.dev/dl/.
- `docker` + `docker compose` — required for Phase 1 local stack and CI integration tests.

**External dependencies (optional / future).**
- `goose` CLI — baked into the Docker image; not needed on the host for normal development.
- `supabase` CLI — used in PR-7/PR-8 for cloud deployment workflows.
- `gh` — GitHub CLI for the delivery agent's PR workflow.

**Hosting.**
- Phase 1: `docker compose up` — local mcp server connecting to Supabase via `DATABASE_URL`.
- Phase 2: Render Free (Docker deploy) connecting to the **same Supabase Free** target.
- Both phases share one DB — only the runtime location of the binary differs.

---

## 2. Repo Map

```
context-harness-mcp/
├── cmd/
│   ├── server/
│   │   └── main.go               Entry point: flag parsing, transport selection, server boot
│   └── khctl/                    Operator CLI: seed, export, import subcommands (Go, no Python/uv)
│       ├── main.go               Subcommand dispatcher
│       ├── dsn.go                DSN resolution (--dsn flag or DATABASE_URL env)
│       ├── seed.go               khctl seed — inserts deterministic dev fixtures
│       ├── export.go             khctl export — SELECTs active rows → JSON
│       └── import.go             khctl import — JSON → Supabase (idempotent merge)
├── internal/                     Go internal packages — not importable by other modules
│   ├── mcp/
│   │   ├── server.go             mcp-go server factory + tool registration scaffold
│   │   ├── nodes.go              create_nodes, add_observations (PR-4; delete tools removed in PR-3)
│   │   ├── relations.go          create_relations (PR-4; delete_relations removed in PR-3)
│   │   └── query.go              search_nodes, open_nodes, read_graph (PR-4)
│   ├── store/                    Postgres + pgvector access (PR-2)
│   │   ├── pool.go               pgxpool + pgvector type registration
│   │   ├── nodes.go              CRUD + soft-delete (admin-script only for deletes)
│   │   ├── observations.go       INSERT with embedding column
│   │   └── relations.go          CRUD + soft-delete
│   ├── embed/                    Embedding pipeline (PR-5)
│   │   ├── model.go              fastembed-go ONNX wrapper; lazy init via sync.Once
│   │   └── tokens.go             Token-count helper for 256-token truncation
│   ├── validate/                 Content Filter — three layers (PR-3)
│   │   ├── syntactic.go          Layer 1: size caps + junk-pattern denylist
│   │   ├── denylist.go           Layer 1: curated junk patterns (in-code table)
│   │   ├── secrets.go            Layer 2: gitleaks-as-library + inline regex fallback
│   │   ├── taxonomy.go           Layer 3: nodeType / relation_type enums
│   │   └── errors.go             Stable policy/* MCP error codes
│   └── healthz/
│       └── healthz.go            healthz MCP tool handler
├── migrations/                   Sequenced SQL migrations applied by goose (PR-2)
├── scripts/
│   └── smoke/                    Operator smoke tests (bash, stay)
├── tests/                        Go integration tests with testcontainers-go ephemeral pg+pgvector (PR-2+)
├── .github/workflows/
│   ├── ci.yml                    On PR: go vet + staticcheck + go build (+ go test from PR-2)
│   ├── deploy.yml                On push to main: goose up + Render deploy hook (PR-7)
│   ├── pg_dump_weekly.yml        Weekly encrypted pg_dump backup (PR-7)
│   └── supabase_keepalive.yml    Every 6 days SELECT 1 (PR-7)
├── Dockerfile                    Multi-stage: golang:1.23 builder → debian:bookworm-slim runtime
├── docker-compose.yml            Phase 1 stack: mcp server only, connects to Supabase via DATABASE_URL
├── go.mod / go.sum               Go module manifest
├── render.yaml                   Render IaC manifest for Phase 2
├── docs/
│   ├── knowledge.md              Durable decisions/patterns/restrictions/stack notes
│   ├── local-stack.md            Phase 1 runbook (PR-6)
│   └── cutover-playbook.md       Phase 2 cutover operator runbook (PR-8)
├── .env.example                  Documented env vars with placeholder values
├── .gitignore
├── CHANGELOG.md
└── CLAUDE.md                     This file
```

---

## 3. Tech Stack

| Layer | Choice |
|---|---|
| Runtime | Go 1.23+, `CGO_ENABLED=1` (required for fastembed-go ONNX bindings) |
| MCP framework | `github.com/mark3labs/mcp-go` v0.47.x — `NewMCPServer`, `AddTool`, `ServeStdio`, `NewStreamableHTTPServer` |
| HTTP server | stdlib `net/http` mux + mcp-go `StreamableHTTPServer` as `http.Handler` |
| DB driver | `github.com/jackc/pgx/v5` — native Postgres protocol, `pgxpool` for connection pooling |
| Vector | `github.com/pgvector/pgvector-go` — `pgxvec.RegisterTypes` in pool `AfterConnect`; cosine via `<=>` |
| Embeddings | `github.com/Anush008/fastembed-go` — `all-MiniLM-L6-v2` ONNX, 384 dims, lazy `sync.Once` init (PR-5) |
| Content Filter | `github.com/go-playground/validator/v10` (struct tags) + `github.com/zricethezav/gitleaks/v8` (secrets, PR-3) |
| Migrations | `github.com/pressly/goose/v3` — forward-only SQL in `migrations/`; same binary for Phase 1 and Phase 2 |
| Auth | `github.com/golang-jwt/jwt/v5` (HS256 issue + validate) + Supabase Auth (user identity) + LRU revocation cache (custom) |
| Logging | stdlib `log/slog` JSON handler to stdout |
| Testing | stdlib `testing` + `github.com/stretchr/testify` + `github.com/testcontainers/testcontainers-go/modules/postgres` (PR-2+); ephemeral pg+pgvector per test run |
| Operator tooling | `cmd/khctl/` — Go binary with `seed`, `export`, `import` subcommands. Shipped in the Docker image at `/usr/local/bin/khctl`. No Python/uv required. |
| Container base | `debian:bookworm-slim` — glibc required for ONNX Runtime Linux x64 |
| Hosting (Phase 1) | `docker compose up` — local mcp server connecting to Supabase via `DATABASE_URL` |
| Hosting (Phase 2) | Render Free (Docker deploy) connecting to the same Supabase Free target |

**Current version:** `0.1.0-dev` (skeleton, PR-1).

---

## 4. Golden Commands

All commands run from the repo root unless noted.

| Intent | Command |
|---|---|
| Build the server binary | `go build ./cmd/server` |
| Run the server (stdio — local Claude Code) | `go run ./cmd/server -transport=stdio` |
| Run the server (http — local browser/test) | `go run ./cmd/server -transport=http -addr=:7654` |
| Run all Go checks | `go vet ./...` |
| Run staticcheck | `go install honnef.co/go/tools/cmd/staticcheck@v0.6.1 && staticcheck ./...` |
| Build and verify | `go build ./... && go vet ./...` |
| Fetch / tidy deps | `GOTOOLCHAIN=local go mod tidy` |
| Start Phase 1 local stack | `docker compose up --build` (requires `DATABASE_URL` in `.env`) |
| Apply migrations to Supabase (local) | `docker compose --profile migrate run --rm migrate` |
| Apply migrations to Supabase (CI) | runs automatically via `.github/workflows/deploy.yml` on push to `main` (PR-7) |
| Run integration tests | `go test ./...  # requires Docker daemon (testcontainers spins ephemeral pg)` |
| Export local KG to JSON | `khctl export --dsn "$DATABASE_URL" --out shared-knowledge/<name>-$(date +%F).json` |
| Seed dev fixtures | `khctl seed --dsn "$DATABASE_URL"` |
| Import KG JSON | `khctl import <file.json> --dsn "$DATABASE_URL"` |
| Run server with auth enabled | `MCP_AUTH=enabled MCP_JWT_SECRET=<hex> MCP_PUBLIC_URL=https://your-host go run ./cmd/server -transport=http -addr=:7654` |
| Sync users with Supabase (admin only) | `khctl sync-users --dsn "$DATABASE_URL" --supabase-service-role-key "$SUPABASE_SERVICE_ROLE_KEY"` |
| Generate JWT secret for rotation | `openssl rand -hex 32` |

**Not applicable to this repo:** `npm`, `pip install`, `python -m`, `uvicorn`, `uv`. The server is Go only; operator tooling is `khctl` (Go binary in the Docker image).

---

## 5. Architectural Conventions

- **Auth via Supabase + JWT HS256.** `auth.Middleware` wraps `/mcp` when `MCP_AUTH=enabled`. Bearer-only header. JWT issued by server, 1-year expiry. See `docs/auth.md`.
- **Revocation.** Supabase Database Webhook + LRU cache TTL 1h with cache-aside invalidation. GH Action cron 6h as fallback (`khctl sync-users`).
- **Attribution.** `created_by_user_id` (uuid) + `created_by_email` (text) nullable on `nodes`/`observations`/`relations`. Stdio + `MCP_AUTH=none` paths persist NULL.
- **Viewer queda público read-only.** `/viewer/*` is unauthenticated by design — locked decision; no cookie auth.
- **Transport selection at boot.** `cmd/server/main.go` parses `-transport` and boots either `ServeStdio` (no network) or a stdlib mux with `StreamableHTTPServer` + a plain `/healthz` handler (HTTP). No other transports.
- **Tool registration in `internal/mcp/server.go`.** Every tool is registered via `RegisterXxx(s *server.MCPServer)` helpers. `main.go` only calls `internalmcp.New()` — it does not import tool packages directly.
- **Content Filter before every write.** Any handler that writes to the DB calls `internal/validate/Run(payload)` first. A rejected payload aborts the whole call — no partial writes. The `pgx.Tx` opens only after `Run` returns `nil`.
- **All deletes are soft.** `deleted_at = now()`. No `DELETE FROM` in any tool handler. Hard deletes are operator-only via Supabase Studio or `psql`.
- **All SQL is parameterized.** No `fmt.Sprintf` inside SQL strings, ever — even next to the validator.
- **Migrations are forward-only in prod.** `goose Down` annotations exist for dev/CI (`goose reset` between tests) but are never invoked in production.
- **ONNX session and gitleaks detector are lazy-loaded.** Both use `sync.Once` — initialized on first embedding / first write request, not at startup. Non-embedding tools (`read_graph`, `open_nodes`) pay no model-load cost.
- **Same Docker image AND same Supabase target for Phase 1 and Phase 2.** Runtime differences live exclusively in `DATABASE_URL`, `MCP_TRANSPORT`, log level. Build artifact drift between phases is a bug.
- **`log/slog` JSON handler only.** No `fmt.Println`, no `log.Printf`, no third-party logging. Structured JSON to stdout; Render and `docker compose` capture stdout.

---

## 6. Mandatory Working Agreements

### 6.1 Pre-work

- Read CLAUDE.md (this file) front to back, including §3 and §4.
- Read the `[Unreleased]` block of CHANGELOG.md to understand in-flight work.
- Read `docs/knowledge.md` for durable decisions, restrictions, and patterns.
- If the change touches the MCP tool surface or the DB schema, also read `session-docs/initial-design/01-architecture.md`.

### 6.2 During-work

- Use a feature branch: `feat/<kebab>`, `fix/<kebab>`, `chore/<kebab>`, `docs/<kebab>`, `refactor/<kebab>`. Never commit to `main`.
- Conventional commits: `feat(area): …`, `fix(area): …`, `docs(area): …`, `refactor(area): …`, `chore(area): …`.
- Never push to `main` directly. Every change ships via pull request.
- Never bypass policy gates (`--no-verify`, `--force`, disabling hooks).

### 6.3 Post-work

- Add a one-line entry under `## [Unreleased]` of CHANGELOG.md.
- If §3 or §4 of CLAUDE.md changed, update them in the same PR.
- If the change establishes a durable decision, pattern, restriction, or stack note, append a one-line bullet to `docs/knowledge.md` with the matching tag prefix.

### 6.4 Governance

- Stop and ask before any irreversible operation (prod data migration, breaking API change, deletion of public surface, force-push to a shared branch).
- Stop and ask when a requirement is ambiguous enough that two interpretations produce visibly different behaviour.
- Stop and ask when the change touches secrets, DB schema, or the MCP tool surface contract.

### 6.5 Anti-patterns

- Never commit secrets, tokens, API keys, `.env` files, or private keys — even temporarily.
- Never `rm -rf` shared paths (`/`, `~`, project root, `.git`). Use targeted scoped paths only.
- Never delete or skip tests to make a build green — fix the code or the test with a documented rationale.
