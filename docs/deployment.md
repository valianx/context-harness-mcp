# Cloud Deployment — Generic Guide

This server is a single static Docker image. It runs **anywhere with Docker + a Postgres+pgvector database** — your dev laptop, any container host, or a self-managed server. This guide covers the principles that apply across every platform, and finishes with short, equally-weighted examples for several popular hosts.

> The local `docker compose up` mode is **not** a Phase 1 stepping stone — it is a first-class deployment mode using the same image. See [`local-stack.md`](local-stack.md). Use cloud hosting when you need shared access for a team, or any deployment exposed beyond localhost.

---

## Deployment principles

Every deployment, regardless of platform, has the same five moving parts:

| Piece | What it is |
|---|---|
| **Container image** | Built from the repo's `Dockerfile`. Multi-stage; runtime base is `debian:bookworm-slim`. Same image for local and cloud. |
| **Environment variables** | `DATABASE_URL` is the only required variable. Auth-related vars are required when `MCP_AUTH=enabled`. Full list below. |
| **Database** | Any Postgres 16+ with the `pgvector` extension. Managed (Supabase, Neon, Railway Postgres, RDS, …) or self-hosted. |
| **Migrations** | `goose up` against `DATABASE_URL`. Same binary in dev, CI, and production. Run once per schema change. |
| **Healthcheck** | `GET /healthz` returns `{"status":"ok"}` over plain HTTP. Wire your host's liveness probe to this URL. |

Optional pieces that some operators wire up:

- **Deploy hook / continuous deploy.** Most container hosts expose a webhook URL that triggers a redeploy. The repo's `.github/workflows/deploy.yml` runs `goose up` then curls a deploy-hook URL on push to `main` — works with any platform that offers one (Render, Railway, Coolify, etc.). If your platform deploys on `git push` natively (Fly machines, Kubernetes via Argo, …), skip the hook step.
- **Encrypted weekly backups.** `.github/workflows/pg_dump_weekly.yml` produces an encrypted `pg_dump` artifact every Sunday. Independent of where the server is hosted.
- **Database keepalive.** Some free-tier Postgres providers auto-pause after a week of inactivity. `.github/workflows/supabase_keepalive.yml` runs `SELECT 1` every 6 days. Adapt or skip per provider.

---

## Required environment variables

| Var | Required | Notes |
|---|---|---|
| `DATABASE_URL` | always | Postgres DSN with `?sslmode=require` for managed providers. `SUPABASE_DB_URL` accepted as a deprecated fallback for one release. |
| `MCP_TRANSPORT` | always (default `http`) | `http` for cloud. `stdio` only when the binary is exec'd by a local Claude Code. |
| `PORT` | PaaS (default `:7654`) | HTTP listen port. Railway / Heroku / Fly / Render set this automatically and route their healthcheck to the same port — leave alone there. Set explicitly only for local docker-compose or bare-metal. |
| `MCP_AUTH` | always (default `none`) | `none` (no bearer required) or `enabled` (bearer JWT required). Garbage values fail fast at boot. |

When `MCP_AUTH=enabled`, the following are also required:

| Var | Notes |
|---|---|
| `MCP_JWT_SECRET` | Hex 32+ bytes. Sign/verify MCP JWTs. Generate via `openssl rand -hex 32`. |
| `MCP_PUBLIC_URL` | Base public URL of the deployed server, e.g. `https://mcp.example.com`. Used in `auth_login_url` error responses. |
| `SUPABASE_PROJECT_URL` | `https://<ref>.supabase.co` — used during `/auth/exchange`. |
| `SUPABASE_ANON_KEY` | Anon JWT (public). Embedded in the callback HTML. |
| `MCP_WEBHOOK_SECRET` | Hex 32+ chars. Verifies the Supabase Database Webhook header. |

Optional:

| Var | Notes |
|---|---|
| `SECRET_MODE` | `reject` (default) or `redact`. Garbage values fail fast at boot. See [`mcp-tools.md`](mcp-tools.md#secret-detection-modes). |
| `MCP_JWT_ISSUER` | Default `context-harness-mcp`. |
| `MCP_JWT_EXPIRY` | Default `8760h` (1 year). |
| `MCP_STDIO_RATE_LIMIT` | Default `1000` writes/s for stdio. |
| `MCP_SNIPPET_SERVER_NAME` | Default `context-harness`. Key used in the snippet rendered by `/auth/login`. |

See [`auth.md`](auth.md) for the full auth runbook and the Database Webhook configuration steps (these are identical regardless of where the server is hosted).

---

## One-time setup (any platform)

1. **Provision Postgres + pgvector.** Create a database on your provider of choice. Enable the `pgvector` extension (most managed providers expose this in their dashboard or via `CREATE EXTENSION vector`).
2. **Apply migrations.** From a workstation with access to the DSN:
   ```sh
   docker compose --profile migrate run --rm migrate
   ```
   …or run `goose -dir migrations postgres "$DATABASE_URL" up` directly. Idempotent.
3. **Decide on auth mode.** Set `MCP_AUTH=none` for a single-operator or fully-trusted deployment. Set `MCP_AUTH=enabled` for team-shared or any deployment reachable beyond localhost. If enabled, finish the Supabase Auth and Database Webhook configuration described in [`auth.md`](auth.md).
4. **Configure your platform's secrets and env vars** with the values from the table above.
5. **Deploy the container.** Either point the platform at this repo's `Dockerfile`, or push the prebuilt image to your registry of choice. The platform should expose port `7654`.
6. **Verify**:
   ```sh
   curl https://<your-host>/healthz   # → {"status":"ok"}
   ```

---

## Continuous deploy

`.github/workflows/deploy.yml` is platform-agnostic: it runs `goose up` against `DATABASE_URL`, then optionally curls a deploy hook URL from `DEPLOY_HOOK_URL` (legacy name: `RENDER_DEPLOY_HOOK_URL` — still read as a fallback for one release).

To wire CD on any host that exposes a webhook URL:

1. Get the deploy-hook URL from your platform.
2. Add a GitHub secret `DEPLOY_HOOK_URL` with that value.
3. Add a GitHub secret `DATABASE_URL` with your DSN.
4. Push to `main` — the workflow runs migrations and triggers the deploy.

If your platform deploys on `git push` natively, you can disable the deploy-hook step and keep only the migrations job (or even skip both and let the platform run migrations as part of its own build pipeline).

---

## Verifying a deploy

```sh
curl https://<your-host>/healthz
# → {"status":"ok"}

# 6 MCP tools round-trip
MCP_URL=https://<your-host>/mcp bash scripts/smoke/happy_path.sh
MCP_URL=https://<your-host>/mcp bash scripts/smoke/secret_rejected.sh
MCP_URL=https://<your-host>/mcp bash scripts/smoke/size_rejected.sh
```

Each script exits 0 and prints `PASS` on success. If `MCP_AUTH=enabled`, set `MCP_BEARER=<jwt>` in the env before running the smokes (see `scripts/smoke/README.md`).

---

## Metrics

The server exposes Prometheus-format metrics at `GET /metrics` when the env var `MCP_EXPOSE_METRICS=1` is set. By default the endpoint is **disabled** — enable it only on deployments where Prometheus can scrape it over an internal network (VPC, private subnet, sidecar). Do not expose `/metrics` publicly without a firewall or network-level access control.

| Env var | Default | Notes |
|---|---|---|
| `MCP_EXPOSE_METRICS` | unset (disabled) | Set to `1` to enable the `/metrics` endpoint. |

### Metrics exposed

| Metric | Type | Labels | Description |
|---|---|---|---|
| `mcp_tool_calls_total` | counter | `tool`, `status` | Tool invocations. `status`: `success`, `error`, `policy_reject`, `rate_limited`. |
| `mcp_tool_duration_seconds` | histogram | `tool` | Tool handler latency. |
| `mcp_content_filter_rejects_total` | counter | `code`, `layer` | Content-Filter rejections by policy code and layer. |
| `mcp_embedder_duration_seconds` | histogram | — | ONNX embedder Encode wall-clock duration. |
| `mcp_jwt_validations_total` | counter | `result` | JWT validation outcomes. `result`: `valid`, `expired`, `invalid_signature`. |
| `mcp_jwt_validation_duration_seconds` | histogram | — | JWT validation latency (signature verify + revocation cache lookup). |

### Prometheus scrape config example

```yaml
scrape_configs:
  - job_name: context-harness-mcp
    static_configs:
      - targets: ["<internal-host>:7654"]
    metrics_path: /metrics
```

---

## Pointing Claude Code at the server

Add to `~/.claude.json` under `mcpServers`:

```json
{
  "mcpServers": {
    "memory": {
      "type": "http",
      "url": "https://<your-host>/mcp"
    }
  }
}
```

With `MCP_AUTH=enabled`, the snippet returned by `/auth/login` also includes the `Authorization: Bearer …` header (see [`auth.md`](auth.md)).

The transport is MCP **streamable-http** (`POST /mcp`). `type: "http"` is correct; `type: "sse"` is the legacy transport and is not supported.

---

## Examples per platform

These examples are **equally-weighted**. The repo does not endorse one host over another — pick whichever fits your team's free-tier budget, region requirements, and operational comfort.

### Railway

1. **New project → Deploy from GitHub repo** → select this repo. Railway auto-detects the `Dockerfile`.
2. **Provision Postgres** as a service in the same project → enable `pgvector` via the Railway shell: `CREATE EXTENSION vector;`. Railway exposes `DATABASE_URL` as a reference variable.
3. **Set env vars** on the MCP service: `MCP_TRANSPORT=http`, `MCP_AUTH=none` (or `enabled` + the auth vars).
4. **Expose** port 7654. Railway generates a `*.up.railway.app` URL.
5. **Healthcheck path**: `/healthz`. **Deploy hook**: Settings → Triggers → Webhook.

### Render

1. **Blueprint** → connect this repo. Render parses the `Dockerfile` and proposes a web service.
2. **Provision Supabase / Neon / Render Postgres** separately. Copy the DSN into the service's **Environment** tab as `DATABASE_URL`.
3. **Auth env vars** (if enabled) go in the same Environment tab.
4. **Deploy hook**: Settings → Deploy Hook → copy the URL. Wire it to `DEPLOY_HOOK_URL` GitHub secret.
5. Free tier has 15-min cold starts; the Go binary cold-starts in 1–3 s once awake.

### Fly.io

1. `fly launch` from the repo root. Pick the region, accept the `Dockerfile`.
2. `fly postgres create` (or attach an external Postgres). `fly postgres attach` injects `DATABASE_URL` automatically.
3. `fly secrets set MCP_TRANSPORT=http MCP_AUTH=enabled MCP_JWT_SECRET=<hex> …` for the rest.
4. **Healthcheck**: configured in `fly.toml` under `[[http_service.checks]]` with `path = "/healthz"`.
5. `fly deploy` on every push, or wire `.github/workflows/deploy.yml` with a custom step calling `fly deploy`.

### Coolify (self-hosted)

1. **New Resource → Docker → Dockerfile** → point at this repo. Coolify clones and builds.
2. **Add a Postgres resource** in the same project. Coolify wires the DSN; enable `pgvector` from the DB shell.
3. **Environment** tab: set the same env vars as above.
4. **Healthcheck**: built into the Coolify UI — path `/healthz`.
5. **Auto-deploy on push** is built in via the Git integration; no separate hook needed.

### Self-hosted Docker (any VPS)

```sh
# On the VPS:
docker network create mcp-net
docker run -d --name pg --network mcp-net \
  -e POSTGRES_PASSWORD=… \
  -v pgdata:/var/lib/postgresql/data \
  pgvector/pgvector:pg16

# Build or pull the image, then:
docker run -d --name mcp --network mcp-net -p 7654:7654 \
  -e DATABASE_URL="postgres://postgres:…@pg:5432/postgres?sslmode=disable" \
  -e MCP_TRANSPORT=http \
  -e MCP_AUTH=enabled \
  -e MCP_JWT_SECRET=… \
  ghcr.io/<your-org>/context-harness-mcp:<tag>
```

Front with Caddy/Nginx/Traefik for TLS. Pointing Claude Code at `https://<your-domain>/mcp` works identically to a managed host.

---

## Free-tier operational concerns

These apply regardless of platform; the workflows in `.github/workflows/` cover them generically.

### Weekly backup

`pg_dump_weekly.yml` runs every Sunday 03:00 UTC. Steps:
1. `pg_dump --no-owner --no-privileges --format=plain "$DATABASE_URL"` produces a plain-SQL dump.
2. `gpg --symmetric --cipher-algo AES256 --passphrase "$DUMP_PASSPHRASE"` encrypts it.
3. Upload as a GH Actions artifact with **90-day retention**.

The encrypted blob is safe to share; the passphrase is not. After 90 days the artifact auto-deletes — for long-term archival, download and copy to durable storage (S3, Glacier, etc.) manually.

### Provider-specific keepalive

Some free-tier Postgres providers auto-pause after several days of inactivity. `.github/workflows/supabase_keepalive.yml` runs a 6-day `SELECT 1` cron — adapt the schedule to your provider's policy, or delete the workflow if your provider does not auto-pause.

### Rate limits and client IP

Write tools are rate-limited to **10 writes per 10 seconds per client identity** (per `sub` claim when authed, per IP otherwise). HTTP hosts typically forward the real client IP via `X-Forwarded-For`; the server reads the leftmost entry. See [`mcp-tools.md`](mcp-tools.md#rate-limit) for the full policy.

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
   psql "$DATABASE_URL" < dump.sql
   ```

> **This OVERWRITES current data.** For partial recovery or when the target DB has diverged, restore into a separate database first, then selectively copy rows.

For an offline cold-spare while cloud recovery is in progress, see the emergency-fallback procedure in [`local-stack.md`](local-stack.md#emergency-fallback) — run a local pgvector container, restore the latest weekly dump into it, and point team members at `http://localhost:7654/mcp` until the cloud DB is back.
