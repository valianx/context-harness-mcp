# Context Harness MCP

An [MCP](https://modelcontextprotocol.io/) server that exposes a Knowledge Graph (nodes, observations, relations) to Claude Code or any MCP-compatible client. Storage is Postgres + [pgvector](https://github.com/pgvector/pgvector); semantic search runs locally via `all-MiniLM-L6-v2` ONNX embeddings. Runs **anywhere with Docker + Postgres+pgvector** — local `docker compose` on a dev machine, or any container host (Railway, Render, Fly, Coolify, self-hosted, …) for team-shared deployments.

## What it does

- **16 MCP tools** to manage a knowledge graph — nodes, relations, query, conflict detection, sessions, observability. See [docs/mcp-tools.md](docs/mcp-tools.md) for the full list.
- **Semantic search** via 384-dim `all-MiniLM-L6-v2` embeddings indexed with pgvector HNSW cosine. `search_nodes("authentication patterns")` returns the nodes whose observations are about auth, not just substring hits.
- **Content Filter** on every write — three layers (size + junk denylist, secrets scan with [gitleaks](https://github.com/gitleaks/gitleaks), taxonomy enforcement) reject payloads with secrets, oversized text, or out-of-taxonomy node/relation types before any DB transaction opens. Atomic reject — never partial writes.
- **MCP-protocol compatible.** Standard MCP streamable-http transport; works with Claude Code and any other MCP-compatible client. JSON wire shapes are stable and documented in [docs/mcp-tools.md](docs/mcp-tools.md).

## Requirements

- **Postgres 16 + pgvector** — the only required data store. Any Postgres 16 instance with the `pgvector` extension works. The easiest free option is a [Supabase](https://supabase.com) project (no credit card); plain self-hosted Postgres is equally valid.
- **Docker** — to run the prebuilt image or build from source with `docker compose`. Alternatively, Go 1.23+ if you want to compile the binaries directly.
- **Embeddings run locally** — semantic search uses a bundled `all-MiniLM-L6-v2` ONNX model. No external embedding API key, no per-call cost. The first image build downloads the ONNX runtime and model (~5 min); the prebuilt `ghcr` image already includes them.
- **Supabase is required only if you enable authentication** (`MCP_AUTH=enabled`). It is the identity provider for the login and user-provisioning flow. With `MCP_AUTH=none` (the default) Supabase is not needed at all.
- **Container image is linux/amd64 only.** The ONNX/CGO build targets amd64; arm64 or other architectures require a local build from source.

## Install

A prebuilt image is published to GitHub Container Registry on every release tag — no local build required:

```sh
docker pull ghcr.io/valianx/context-harness-mcp:latest
# or pin to a specific release:
docker pull ghcr.io/valianx/context-harness-mcp:1.0.0
```

Use the prebuilt image in place of `docker compose up --build` in the options below. The image is published automatically via `.github/workflows/release.yml` using `GITHUB_TOKEN` — no additional credentials needed.

### Option A — Local (Docker)

You need Docker and a reachable Postgres with the `pgvector` extension. The easiest target is a free [Supabase](https://supabase.com) project (no credit card), but any Postgres 16 + pgvector works.

```sh
git clone https://github.com/valianx/context-harness-mcp
cd context-harness-mcp
cp .env.example .env
# Edit .env and set DATABASE_URL to your Postgres DSN.

docker compose --profile migrate run --rm migrate   # apply schema (one-time)
docker compose up                                    # start the server
```

The server listens on `http://localhost:7654/mcp`. First build takes ~5 minutes (ONNX runtime + model download); subsequent ups are instant.

To use it from Claude Code, add to `~/.claude.json`:

```json
{
  "mcpServers": {
    "memory": {
      "type": "http",
      "url": "http://localhost:7654/mcp"
    }
  }
}
```

Full local runbook — DB options, migrations, troubleshooting, smoke tests — in [docs/local-stack.md](docs/local-stack.md).

**Authentication toggle:** by default the server starts with `MCP_AUTH=none` — the `/mcp` endpoint is public and no Supabase project is required. To enable bearer-token auth, set `MCP_AUTH=enabled` plus `MCP_JWT_SECRET`, `SUPABASE_PROJECT_URL`, `SUPABASE_ANON_KEY`, and `MCP_WEBHOOK_SECRET`. See [docs/auth.md](docs/auth.md) for the full setup runbook. Do not expose a public-internet deployment without auth enabled.

### Option B — Cloud (any container hosting + any Postgres+pgvector)

Same Docker image, deployed to whatever container host you prefer ([Railway](https://railway.app), [Render](https://render.com), [Fly](https://fly.io), [Coolify](https://coolify.io), your own VPS, …) and pointed at any Postgres 16+ with `pgvector` (managed: Supabase, Neon, Railway Postgres, RDS, …; or self-hosted). Continuous deploy via the included GitHub workflows (`goose up` + optional deploy hook), weekly encrypted `pg_dump` backups, and a configurable keepalive cron for providers that auto-pause.

One-time setup runbook — principles plus equally-weighted per-platform examples — in [docs/deployment.md](docs/deployment.md).

## Limitations

- **Single-tenant by design.** One deployment = one shared Knowledge Graph = one team. All users of an instance share the same graph; `project` is a grouping tag, not an access-control boundary. For multiple isolated teams, run multiple independent deployments — each with its own Postgres database, its own Supabase project (if auth is enabled), and its own `MCP_JWT_SECRET`.
- **Authentication is off by default.** `MCP_AUTH=none` means `/mcp` is fully public. The server emits a recurring log warning when auth is off and a remote DB is configured, but it does not block requests. Enable auth before any public-internet deployment.
- **Auth login flow is Supabase-only.** The token itself is a standard HS256 JWT validated locally; but the login, user-provisioning, and revocation flow is written against Supabase. No other identity provider is supported without code changes. See [docs/auth.md](docs/auth.md) for the architecture details.
- **No hard-delete via the API.** Permanent removal requires direct SQL or the Supabase Studio dashboard. The API provides reversible soft-delete via `mark_superseded`.
- **Revocation fails open on DB error.** If a database outage occurs during a token-revocation check, the server trusts a recent cached result rather than denying the request. This is a deliberate availability-over-strictness trade-off.

## Documentation

| File | What's in it |
|---|---|
| [docs/mcp-tools.md](docs/mcp-tools.md) | The 16 MCP tools — arguments, responses, examples, error codes. |
| [docs/local-stack.md](docs/local-stack.md) | Local Docker runbook (Option A above, expanded). |
| [docs/deployment.md](docs/deployment.md) | Cloud deployment runbook (Option B above, expanded). |
| [docs/auth.md](docs/auth.md) | Auth runbook — Supabase setup, dev flow, webhook config, revocation, troubleshooting. |
| [docs/knowledge.md](docs/knowledge.md) | Durable decisions, constraints, patterns, stack notes. |
| [CHANGELOG.md](CHANGELOG.md) | Release notes (Keep-a-Changelog format). |
| [CLAUDE.md](CLAUDE.md) | Repo conventions for AI-assisted contributors. |

## Tech stack

Go 1.23 + [`mcp-go`](https://github.com/mark3labs/mcp-go) + [`pgx/v5`](https://github.com/jackc/pgx) + [`pgvector-go`](https://github.com/pgvector/pgvector-go) + [`fastembed-go`](https://github.com/Anush008/fastembed-go) (ONNX). Migrations via [`goose`](https://github.com/pressly/goose). Tests via [`testcontainers-go`](https://golang.testcontainers.org/). Operator tooling (`khctl`) is also Go — no Python/uv runtime required.

## Repo conventions

- Conventional commits (`feat(area): …`, `fix(area): …`, …).
- Feature branches: `feat/<kebab>`, `fix/<kebab>`, `docs/<kebab>`, `chore/<kebab>`, `refactor/<kebab>`.
- Never commit on `main`. Every change ships via pull request.
- No secrets in the repo. Ever.

Full working agreements in [CLAUDE.md §6](CLAUDE.md).

## License

[MIT](./LICENSE) © 2026 Mario Gutierrez. Contributions welcome — open an issue or PR.
