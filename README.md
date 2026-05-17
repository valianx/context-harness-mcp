# context-harness-mcp

> MCP server exposing a Knowledge-Graph surface (entities, observations, relations) backed by Supabase Postgres with `pgvector`-powered semantic search.

**Status:** Stage 1 (architectural design) — no implementation yet.

**Intent.** Eventual remote replacement for the local-ChromaDB knowledge-graph MCP shipped by [`claude-dev-team`](../claude-dev-team). Same MCP tool surface (`search_nodes`, `read_graph`, `open_nodes`, `create_entities`, `add_observations`, `delete_entities`, `delete_observations`, `create_relations`, `delete_relations`), drop-in schema, migration path from existing ChromaDB exports.

**Design artifacts.** During Stage 1 the architectural deliverables live in `session-docs/initial-design/` (this folder is git-ignored — pre-merge artifacts only). The polished design will graduate to `docs/` as part of the first implementation PR.

## Repo conventions

- Conventional commits.
- Feature branches: `feat/<kebab>`, `fix/<kebab>`, `docs/<kebab>`, `chore/<kebab>`, `refactor/<kebab>`.
- Never commit on `main` directly — every change ships via PR.
- No secrets in the repo, ever.
