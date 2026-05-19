# Context Harness MCP

An [MCP](https://modelcontextprotocol.io/) server that exposes a Knowledge Graph (nodes, observations, relations) to Claude Code or any MCP-compatible client. Storage is Postgres + [pgvector](https://github.com/pgvector/pgvector); semantic search runs locally via `all-MiniLM-L6-v2` ONNX embeddings. Designed to run at **$0/month** on Render Free + Supabase Free, or fully offline via `docker compose`.

## What it does

- **6 MCP tools** to manage a knowledge graph — `create_nodes`, `add_observations`, `create_relations`, `search_nodes`, `open_nodes`, `read_graph`. See [docs/mcp-tools.md](docs/mcp-tools.md).
- **Semantic search** via 384-dim `all-MiniLM-L6-v2` embeddings indexed with pgvector HNSW cosine. `search_nodes("authentication patterns")` returns the nodes whose observations are about auth, not just substring hits.
- **Content Filter** on every write — three layers (size + junk denylist, secrets scan with [gitleaks](https://github.com/gitleaks/gitleaks), taxonomy enforcement) reject payloads with secrets, oversized text, or out-of-taxonomy node/relation types before any DB transaction opens. Atomic reject — never partial writes.
- **Drop-in compatible** with the local-ChromaDB knowledge graph shipped by [`claude-dev-team`](https://github.com/valianx/claude-dev-team). Same tool surface, same JSON wire shapes, [migration tooling included](docs/cutover-playbook.md).

## Install

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

### Option B — Cloud (Render Free + Supabase Free)

Same Docker image, deployed to [Render](https://render.com) and pointed at a [Supabase](https://supabase.com) project. Both providers offer free tiers that together run the full stack at $0/month, with continuous deploy on push to `main`, weekly encrypted `pg_dump` backups, and a 6-day Supabase keepalive to dodge auto-pause.

One-time setup runbook in [docs/deployment.md](docs/deployment.md).

## Documentation

| File | What's in it |
|---|---|
| [docs/mcp-tools.md](docs/mcp-tools.md) | The 6 MCP tools — arguments, responses, examples, error codes. |
| [docs/local-stack.md](docs/local-stack.md) | Local Docker runbook (Option A above, expanded). |
| [docs/deployment.md](docs/deployment.md) | Cloud deployment runbook (Option B above, expanded). |
| [docs/cutover-playbook.md](docs/cutover-playbook.md) | Migrating from `claude-dev-team`'s local ChromaDB to this server. |
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

Not yet declared. Treat as proprietary until a `LICENSE` file lands.
