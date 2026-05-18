# Cloud Deployment — Render Free + Supabase Free

Phase 2 of the deployment model: the same Docker image validated by Phase 1 (`docker compose up`) deployed to [Render](https://render.com) and pointed at a [Supabase](https://supabase.com) project. Total cost at the time of writing: **$0/month**, with three free-tier compromises documented at the bottom.

## Architecture overview

| Component | Where | Purpose |
|---|---|---|
| `mcp` container | Render Free (web service, Oregon region) | Runs the Go server, listens on `:8080`, serves `/mcp` (MCP streamable-http) and `/healthz`. |
| Postgres + pgvector | Supabase Free | The actual KG storage. Schema lives in `migrations/`, applied by goose. |
| Continuous Deploy | `.github/workflows/deploy.yml` | On every push to `main`: `goose -dir migrations postgres "$SUPABASE_DB_URL" up`, then curl the Render deploy hook. |
| Weekly backup | `.github/workflows/pg_dump_weekly.yml` | Sundays 03:00 UTC: `pg_dump` → `gpg --symmetric` (AES-256) → upload as GH Actions artifact, 90-day retention. |
| Keepalive | `.github/workflows/supabase_keepalive.yml` | Every 6 days: `psql -c 'SELECT 1'` against Supabase to dodge the 7-day auto-pause on Free tier. |

The Docker image, the `migrations/` directory, and the `goose` binary are byte-identical across Phase 1 and Phase 2. Only `SUPABASE_DB_URL` differs.

---

## One-time setup

Complete steps 1–5 once. After step 5, every push to `main` triggers the full CD pipeline automatically.

### 1. Provision a Supabase project

Create a free project at [supabase.com](https://supabase.com) (no credit card required). Go to **Project Settings → Database → Connection string (URI)** and copy the URI. It looks like:

```
postgres://postgres:<password>@db.<project-ref>.supabase.co:5432/postgres?sslmode=require
```

### 2. Provision the Render service

In the Render dashboard: **New + → Blueprint → connect this repo → review `render.yaml`**. Render parses `render.yaml` and proposes one `context-harness-mcp` web service. Approve it.

During or right after the first deploy, fill in the `SUPABASE_DB_URL` env var manually in the service's **Environment** tab. It's declared `sync: false` in `render.yaml` — the Render idiom for "set this in the dashboard; never source from git."

### 3. Copy the Render deploy hook URL

In the Render service: **Settings → Deploy Hook → copy the URL**. It looks like `https://api.render.com/deploy/srv-<id>?key=<token>`. Treat it as a secret — the URL alone triggers a deploy.

### 4. Generate a dump passphrase

```sh
openssl rand -base64 32
```

Save the output in a password manager. **Without this passphrase the encrypted `pg_dump` artifacts are unrecoverable.** There is no recovery path.

### 5. Set the GitHub secrets

At **https://github.com/valianx/context-harness-mcp/settings/secrets/actions**, add:

| Secret | Value | Used by |
|---|---|---|
| `SUPABASE_DB_URL` | DSN from step 1 (with `sslmode=require`) | `deploy.yml`, `pg_dump_weekly.yml`, `supabase_keepalive.yml` |
| `RENDER_DEPLOY_HOOK_URL` | URL from step 3 | `deploy.yml` |
| `DUMP_PASSPHRASE` | Generated in step 4 | `pg_dump_weekly.yml` |
| `SUPABASE_ACCESS_TOKEN` | Supabase personal access token | Not used by current workflows — reserved for future Supabase-CLI integration |

### 6. Trigger the first deploy

Push any commit to `main`, or use the **workflow_dispatch** button on the **Deploy** workflow in the GH Actions UI. `deploy.yml` runs `goose up` then curls the Render hook.

---

## What happens on each push to `main`

1. `deploy.yml` checks that `SUPABASE_DB_URL` and `RENDER_DEPLOY_HOOK_URL` are set. If either is missing, the workflow emits a `::warning::` and exits 0 — no deploy.
2. `goose -dir migrations postgres "$SUPABASE_DB_URL" up` runs. Idempotent — "no migrations to run" is a success.
3. The Render deploy hook is called (`POST`). Render pulls the latest image and deploys it.

Migrations run **before** the deploy hook. If migrations fail, the Render deploy is not triggered.

---

## GH secrets reference

| Secret | Workflow(s) | Missing → |
|---|---|---|
| `SUPABASE_DB_URL` | `deploy.yml`, `pg_dump_weekly.yml`, `supabase_keepalive.yml` | Deploy, backup, and keepalive all skip |
| `RENDER_DEPLOY_HOOK_URL` | `deploy.yml` | Deploy skipped even if migrations succeed |
| `DUMP_PASSPHRASE` | `pg_dump_weekly.yml` | Weekly backup skipped |
| `SUPABASE_ACCESS_TOKEN` | (none) | No effect — reserved |

Every workflow has a guard step that exits 0 cleanly when its required secrets are absent. They show as ✓ in the UI with a yellow `::warning::`. This lets the workflows land in the repo before the operator finishes the one-time setup without producing noisy failures.

---

## Verifying a deploy worked

### Check the Render dashboard

The latest deploy in the service should show **Live** (green). Render Free cold-starts after 15 min of inactivity; warm requests respond in 1–3 s (Go static binary).

### Curl the health endpoint

```sh
curl https://<your-render-url>.onrender.com/healthz
```

Expect HTTP 200 with `{"status":"ok"}`. A 502 or timeout usually means a cold start in progress — wait 15–30 s and retry.

### Run the smoke tests against the live URL

```sh
MCP_URL=https://<your-render-url>.onrender.com/mcp bash scripts/smoke/happy_path.sh
MCP_URL=https://<your-render-url>.onrender.com/mcp bash scripts/smoke/secret_rejected.sh
MCP_URL=https://<your-render-url>.onrender.com/mcp bash scripts/smoke/size_rejected.sh
```

Each script exits 0 and prints `PASS` on success.

---

## Pointing Claude Code at the Render URL

Add the following to `~/.claude.json` under `mcpServers`, replacing `<your-render-url>` with the Render subdomain:

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

Restart Claude Code. All 9 MCP tools (`read_graph`, `search_nodes`, the writers, the deleters) will route through the Render-hosted server. On the first call after a cold start, allow up to 30 s for the server to wake.

> The server uses the MCP **streamable-http** transport (`POST /mcp`). `type: "http"` is correct; `type: "sse"` is the legacy transport and is not supported.

---

## Free-tier operational model

### Weekly backup

`pg_dump_weekly.yml` runs every Sunday 03:00 UTC. Steps:
1. `pg_dump --no-owner --no-privileges --format=plain "$SUPABASE_DB_URL"` produces a plain-SQL dump.
2. `gpg --symmetric --cipher-algo AES256 --passphrase "$DUMP_PASSPHRASE"` encrypts it.
3. Upload as a GH Actions artifact with **90-day retention**.

The encrypted blob is safe to share; the passphrase is not. After 90 days the artifact auto-deletes — for long-term archival, download and copy to durable storage (S3, Glacier, etc.) manually.

### 6-day keepalive

`supabase_keepalive.yml` runs every 6 days at 12:00 UTC, executes `SELECT 1`. Supabase Free auto-pauses after 7 days of DB inactivity; the 6-day cadence is a 1-day safety buffer.

### Trade-offs (vs paid tier)

- **No PITR.** Recovery window is up to 7 days (one backup cycle). For a tighter RPO, trigger a manual `workflow_dispatch` run of **Weekly pg_dump** before any risky operation.
- **Cold starts of 15 min.** Render Free spins down idle services. First request after idle takes 15–30 s.
- **No SLA.** Both Render Free and Supabase Free are best-effort. Real production deployments should sit on paid tiers.

Captured as a `[decisión]` in [`docs/knowledge.md`](knowledge.md): "Costo mensual recurrente $0".

---

## Recovery from a backup

1. Download the `pg_dump-encrypted` artifact from the GH Actions UI (most recent successful run of **Weekly pg_dump**).
2. Decrypt:
   ```sh
   gpg --decrypt context-harness-mcp-<timestamp>.sql.gpg > dump.sql
   ```
   Enter the `DUMP_PASSPHRASE` when prompted (or pass `--passphrase` for scripted recovery).
3. Restore:
   ```sh
   psql "$SUPABASE_DB_URL" < dump.sql
   ```

> **This OVERWRITES current data.** For partial recovery or when the target DB has diverged, restore into a separate Supabase project first, then selectively copy rows.

For incident-grade recovery procedures (rollback criteria, secret rotation, emergency local fallback), see [`docs/cutover-playbook.md`](cutover-playbook.md).
