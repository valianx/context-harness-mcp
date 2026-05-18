# migrations/

SQL migrations for `context-harness-mcp`, applied via [goose v3](https://github.com/pressly/goose).

## File naming

```
NNNNN_description.sql
```

Files are applied in ascending numeric order. Never edit a migration after it has been applied to any environment — add a new migration instead.

## Application paths

There are two paths for applying migrations against **Supabase**. Tests use a third path that is completely separate (see below).

### 1. Manual dev — docker compose migrate profile

```bash
# from the repo root (requires SUPABASE_DB_URL in .env)
docker compose --profile migrate run --rm migrate
```

This runs the `migrate` sidecar container (defined in `docker-compose.yml`) against Supabase Free via `SUPABASE_DB_URL`. Use this when you add a new migration file and want to verify it applies cleanly before opening a PR.

### 2. Automated CI/CD — deploy.yml (lands in PR-7)

On every push to `main`, `.github/workflows/deploy.yml` runs:

```bash
goose -dir migrations postgres "$SUPABASE_DB_URL" up
```

using the goose binary baked into the Docker image. This is the production migration path. No manual intervention is required after merging to `main`.

## Why not `supabase db push`?

`supabase db push` is a Supabase-specific command that requires the Supabase CLI and operates on its own migration ledger (different from goose's `goose_db_version` table). Using it would mean maintaining **two parallel migration runtimes** — one for the local docker-compose stack and one for Supabase CI — which defeats the "single source of truth" goal. `goose up` works identically against any Postgres target, which is why it was chosen. See `docs/knowledge.md` `[stack]` entry for the full rationale.

## Test path (completely separate from the above)

Integration tests in `tests/` do **not** use docker-compose or the Supabase CLI. They use [testcontainers-go](https://golang.testcontainers.org/modules/postgres/) to spin up an ephemeral `pgvector/pgvector:pg16` container per test run, then call `goose.Up` via the goose Go library directly against that container. This guarantees:

- Tests are self-contained and reproducible without any external service.
- The same migration files applied by docker-compose and CI are also applied by the test harness — no migration drift between paths.
- `go test ./...` works on any machine with a Docker daemon, without a Supabase account.

## Ordering rules

- **Forward-only in production.** Migrations are always applied with `goose up`. The `-- +goose Down` annotations exist for development use only (`goose reset` between tests, `goose down` to roll back locally after a mistake).
- **Never invoke `goose down` in production** (i.e., in `deploy.yml` or any CI/CD step against Supabase).
- **Additive-only schema changes.** Adding columns, indexes, and tables is safe. Dropping or renaming columns requires an additive migration cycle: add new column → backfill → switch reads → drop old column in a later release.
- **Gap-free sequence.** Migration numbers must be contiguous. `goose` will refuse to apply a migration if a lower-numbered one has not been applied.

## Current migrations

| File | Description |
|------|-------------|
| `00001_init.sql` | pgvector extension; `entities`, `observations`, `relations` tables; HNSW cosine index on `observations.embedding`; CHECK constraints on `entity_type` (9 values) and `relation_type` (5 values); unique constraints for dedup; soft-delete columns (`deleted_at`) on all three tables. |
| `00002_soft_delete.sql` | Additional partial indexes on `deleted_at IS NULL` for active-row lookup paths; `v_active_entities` restore helper view. |
