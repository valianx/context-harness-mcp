# context-harness-mcp

> MCP server exposing a Knowledge-Graph surface (entities, observations, relations) backed by Supabase Postgres with `pgvector`-powered semantic search.

**Status:** Phase 1 + Phase 2 live (as of PR-7). Stage 1 design archived in `session-docs/initial-design/` (git-ignored — pre-merge artifacts only).

**Intent.** Remote replacement for the local-ChromaDB knowledge-graph MCP shipped by [`claude-dev-team`](../claude-dev-team). Same MCP tool surface (`search_nodes`, `read_graph`, `open_nodes`, `create_entities`, `add_observations`, `delete_entities`, `delete_observations`, `create_relations`, `delete_relations`), drop-in schema, migration path from existing ChromaDB exports.

## Repo conventions

- Conventional commits.
- Feature branches: `feat/<kebab>`, `fix/<kebab>`, `docs/<kebab>`, `chore/<kebab>`, `refactor/<kebab>`.
- Never commit on `main` directly — every change ships via PR.
- No secrets in the repo, ever.

## Quickstart

See [`docs/local-stack.md`](docs/local-stack.md) for the Phase 1 local-docker-compose runbook: quick start, DB options, migrations, smoke tests, and Claude Code wiring against `http://localhost:8080/mcp`.

---

## Deployment

### Architecture overview

**Phase 1** — `docker compose up` local. The `mcp` service runs against any reachable Postgres with `pgvector`. Default target: Supabase Free (same DB as Phase 2).

**Phase 2** — same Docker image on Render Free, against the same Supabase Free DB. CI (`ci.yml`) runs the same image against an ephemeral `pgvector/pgvector:pg16` service container on every pull request.

Migrations in `migrations/` are the single source of schema truth, applied by goose from two points: the `migrate` docker-compose profile (manual, dev local) and `deploy.yml` (automatic, push to `main`).

---

### One-time setup

Complete these steps once before the first real deploy. After step 5, every push to `main` triggers the full CD pipeline automatically.

**1. Provision a Supabase project.**

Create a free project at https://supabase.com (no credit card required). Navigate to **Project Settings → Database → Connection string (URI)** and copy the connection string. It looks like:

```
postgres://postgres:<password>@db.<project-ref>.supabase.co:5432/postgres?sslmode=require
```

**2. Provision the Render service.**

In the Render dashboard: **New + → Blueprint → connect this repo → review `render.yaml`**. Render reads `render.yaml` and creates a `context-harness-mcp` web service automatically.

During or immediately after the first deploy, fill in the `SUPABASE_DB_URL` environment variable manually in the service's **Environment** tab. It is marked `sync: false` in `render.yaml`, which is the Render idiom for "set this in the dashboard; never source from git."

**3. Copy the Render deploy hook URL.**

In the Render service: **Settings → Deploy Hook → copy the URL**. It looks like `https://api.render.com/deploy/srv-<id>?key=<token>`. Keep it safe — it triggers a deploy without authentication beyond the URL itself.

**4. Generate a dump passphrase.**

```sh
openssl rand -base64 32
```

Copy the output. Save it in a password manager — without it the encrypted `pg_dump` artifacts are unrecoverable. There is no recovery path for a lost passphrase.

**5. Set the GitHub secrets.**

Navigate to **https://github.com/valianx/context-harness-mcp/settings/secrets/actions** and add:

| Secret name | Value | Required by |
|---|---|---|
| `SUPABASE_DB_URL` | Full Postgres DSN with `sslmode=require` | `deploy.yml`, `pg_dump_weekly.yml`, `supabase_keepalive.yml` |
| `RENDER_DEPLOY_HOOK_URL` | Copied from step 3 | `deploy.yml` |
| `DUMP_PASSPHRASE` | Generated in step 4 | `pg_dump_weekly.yml` |
| `SUPABASE_ACCESS_TOKEN` | Supabase personal access token | Not used by current workflows — optional, reserved for future Supabase CLI integration |

**6. Trigger the first deploy.**

Either push a no-op commit to `main` or use the **workflow_dispatch** button on the **Deploy** workflow in the GH Actions UI. The workflow will run `goose up` against your Supabase DB, then curl the Render deploy hook.

---

### What happens on each push to main

1. `deploy.yml` checks that `SUPABASE_DB_URL` and `RENDER_DEPLOY_HOOK_URL` are set. If either is missing, the workflow exits 0 with a `::warning::` in the log pointing at this runbook — no deployment occurs.
2. `goose -dir migrations postgres "$SUPABASE_DB_URL" up` runs. Migrations are idempotent; "no migrations to run" is a success.
3. The Render deploy hook is curled (`POST`). Render pulls the latest image from the linked registry and deploys it.

The migration step runs **before** the deploy hook. If migrations fail, no Render deploy is triggered.

---

### GH secrets reference

| Secret | Workflow(s) | Missing → |
|---|---|---|
| `SUPABASE_DB_URL` | `deploy.yml`, `pg_dump_weekly.yml`, `supabase_keepalive.yml` | Deploy and backup skipped; keepalive skipped |
| `RENDER_DEPLOY_HOOK_URL` | `deploy.yml` | Deploy skipped even if migrations succeed |
| `DUMP_PASSPHRASE` | `pg_dump_weekly.yml` | Weekly backup skipped |
| `SUPABASE_ACCESS_TOKEN` | (none currently) | No effect — reserved for future use |

All three active workflows have a guard step that exits 0 cleanly when their required secrets are absent. Workflows show as ✓ in the UI with a yellow `::warning::` in the log. This allows the workflows to land in the repo before the user completes the one-time setup without producing noisy failures.

---

### Verifying a deploy worked

**1. Check the Render dashboard.**

Navigate to the service and confirm the latest deploy shows a green **Live** status. Cold starts on Render Free can take up to 15 minutes of inactivity; subsequent requests within the activity window respond in 1–3 seconds (Go static binary).

**2. Curl the health endpoint.**

```sh
curl https://<your-render-url>.onrender.com/healthz
```

Expected response (HTTP 200):

```json
{"status":"ok"}
```

If you get a 502 or timeout, the service may be cold-starting. Wait 15–30 seconds and retry.

**3. Run the smoke tests against the Render URL.**

```sh
MCP_URL=https://<your-render-url>.onrender.com/mcp bash scripts/smoke/happy_path.sh
MCP_URL=https://<your-render-url>.onrender.com/mcp bash scripts/smoke/secret_rejected.sh
MCP_URL=https://<your-render-url>.onrender.com/mcp bash scripts/smoke/size_rejected.sh
```

Each script exits 0 and prints `PASS` on success.

---

### Pointing Claude Code at the Render URL

Add the following entry to `~/.claude.json` under `mcpServers`, replacing `<your-render-url>` with your actual Render subdomain:

```json
{
  "mcpServers": {
    "memory": {
      "type": "http",
      "url": "https://<your-render-url>.onrender.com/mcp"
    }
  }
}
```

Restart Claude Code after editing. The `read_graph`, `search_nodes`, and all write tools will now call the Render-hosted server. On the first call after a cold start, allow up to 30 seconds for the server to respond.

> **Note:** The server uses the MCP Streamable-HTTP transport (`POST /mcp`). The `type: "http"` entry is correct; `type: "sse"` is the legacy transport and is not supported.

---

### Free-tier operational model

**Weekly backup:** `pg_dump_weekly.yml` runs every Sunday at 03:00 UTC, produces a plain-SQL dump, encrypts it with AES-256 using `DUMP_PASSPHRASE`, and uploads it as a GH Actions artifact with 90-day retention. The encrypted blob is safe to share; the passphrase is not. After 90 days the artifact auto-deletes — for long-term archival, download and copy to durable storage (S3, Glacier, etc.) manually.

**6-day keepalive:** `supabase_keepalive.yml` runs every 6 days at 12:00 UTC and executes `SELECT 1` against Supabase. Supabase Free auto-pauses after 7 days of DB inactivity; the 6-day cadence provides a 1-day safety buffer.

This op model trades PITR and SLA guarantees for zero recurring hosting cost. See `docs/knowledge.md` for the accepted trade-offs (`[decisión]` bullet: "Costo mensual recurrente $0").

---

### Recovery from a backup

1. Download the `pg_dump-encrypted` artifact from the GH Actions UI (the most recent successful run of **Weekly pg_dump**).
2. Decrypt:
   ```sh
   gpg --decrypt context-harness-mcp-<timestamp>.sql.gpg > dump.sql
   ```
   Enter the `DUMP_PASSPHRASE` when prompted (or pass `--passphrase` for scripted recovery).
3. Restore:
   ```sh
   psql "$SUPABASE_DB_URL" < dump.sql
   ```
   > **Warning:** This OVERWRITES current data. For partial recovery or if the target DB has diverged, restore into a separate Supabase project first, then selectively copy rows.

Recovery window is up to 7 days (one weekly backup cycle). For a tighter RPO, trigger a manual `workflow_dispatch` run of **Weekly pg_dump** before any risky operation.
