# Local Stack — context-harness-mcp

Runbook for running the MCP server locally via `docker compose`. Covers quick start, DB target configuration, migrations, smoke tests, Claude Code wiring, and troubleshooting.

> **Local mode is a first-class deployment.** The exact same Docker image runs locally and on any container host you choose for cloud/team-shared deployments — see [`deployment.md`](deployment.md). Local mode is the right choice for single-developer setups, dev machines, and fully-trusted environments where `MCP_AUTH=none` is acceptable.

---

## Quick start

```sh
git clone https://github.com/valianx/context-harness-mcp
cd context-harness-mcp
cp .env.example .env          # then fill in DATABASE_URL (see §Configuring your DB target)
docker compose up -d --wait   # builds the image, starts the mcp service, waits for healthy
curl http://localhost:7654/healthz
# → {"status":"ok","db":"not-configured"}
```

The server is now reachable at `http://localhost:7654/mcp`.

---

## Configuring your DB target

`DATABASE_URL` is the only required env var. Point it at any Postgres instance with the `pgvector` extension enabled.

### Option A — Supabase Free (recommended)

1. Create a free project at https://supabase.com/ (no credit card required).
2. Navigate to **Project Settings → Database → Connection string** and copy the URI.
3. Paste it into `.env`:
   ```
   DATABASE_URL=postgres://postgres:<password>@db.<project-ref>.supabase.co:5432/postgres?sslmode=require
   ```

### Option B — Local pgvector container

Run a pgvector container managed independently of `docker-compose.yml`:

```sh
docker run --name pg-local \
    -e POSTGRES_PASSWORD=devpw \
    -p 5432:5432 \
    -d pgvector/pgvector:pg16
```

Then set `.env`:

```
DATABASE_URL=postgres://postgres:devpw@host.docker.internal:5432/postgres?sslmode=disable
```

> **Linux note:** `host.docker.internal` resolves on Docker Desktop for Mac/Windows. On Linux, use the host bridge IP (`172.17.0.1` by default) or add `--network host` to the `mcp` service. Alternatively, add `extra_hosts: ["host.docker.internal:host-gateway"]` to the mcp service in a local `docker-compose.override.yml`.

---

## Applying migrations

Run goose migrations against your DB target before starting the server for the first time, or after pulling new migrations:

```sh
docker compose --profile migrate run --rm migrate
```

This executes `goose -dir /migrations postgres "$DATABASE_URL" up` inside the same image as the server — the `migrations/` directory is baked into the image at `/migrations` (see `Dockerfile`).

Migrations are idempotent: running the command again when already up-to-date prints "no migrations to run" and exits 0.

---

## Running smoke tests

With the stack up (`docker compose up -d --wait`) and migrations applied:

```sh
bash scripts/smoke/happy_path.sh        # create_nodes/read_graph round-trip
bash scripts/smoke/secret_rejected.sh   # AWS-key observation → policy/secret-detected
bash scripts/smoke/size_rejected.sh     # 65KB observation → policy/size-exceeded
```

To run against a non-default MCP URL (e.g., a remote container host or a dev box):

```sh
MCP_URL=https://your-host.example.com/mcp bash scripts/smoke/happy_path.sh
```

Each script exits 0 on success and prints a clear `PASS` or `FAIL` line at the end.

---

## Pointing Claude Code at the local MCP

Add the following entry to `~/.claude.json` under `mcpServers`:

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

Restart Claude Code after editing. The `read_graph`, `search_nodes`, `open_nodes`, `create_nodes`, `add_observations`, and `create_relations` tools will now call your local server. Run `docker compose up -d --wait` before starting Claude Code.

> **Note:** The server uses the MCP Streamable-HTTP transport (`POST /mcp`). The `type: "http"` entry is correct; `type: "sse"` is the legacy transport and is not supported by this server.

---

## Emergency fallback

`docker-compose.yml` does not include a local Postgres service by design (see CHANGELOG.md for the rationale). If your remote Postgres provider is paused or unavailable, you can run a local pgvector container manually as a stop-gap:

1. **Start a pgvector container:**
   ```sh
   docker run --name pg-fallback \
       -e POSTGRES_PASSWORD=fallbackpw \
       -p 5432:5432 \
       -d pgvector/pgvector:pg16
   ```

2. **Load the latest weekly dump** (see PR-7 for the `pg_dump_weekly.yml` workflow that produces the artifact):
   ```sh
   gpg --decrypt dump-YYYY-MM-DD.sql.gpg | psql "postgres://postgres:fallbackpw@localhost:5432/postgres"
   ```

3. **Update `.env`** to point at the local container:
   ```
   DATABASE_URL=postgres://postgres:fallbackpw@host.docker.internal:5432/postgres?sslmode=disable
   ```

4. **Restart the stack:**
   ```sh
   docker compose up -d --wait
   ```

5. **Redirect team members** to `http://localhost:7654/mcp` while cloud recovery is in progress.

---

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| `docker compose build` fails | Stale build cache or dependency change | `docker compose build --no-cache mcp` |
| Container starts then exits immediately | `DATABASE_URL` not set or malformed | Check `.env` file; confirm DSN is reachable with `psql "$DATABASE_URL" -c 'select 1'` |
| `Error response from daemon: ... port 7654 already in use` | Another process bound to port 7654 | `docker compose down` and check `lsof -i :7654` (or `netstat -ano` on Windows) |
| `healthcheck` fails repeatedly, service stays `unhealthy` | DB connection refused | Apply migrations first (`docker compose --profile migrate run --rm migrate`) and verify `DATABASE_URL` |
| `libonnxruntime.so: cannot open shared object file` | Missing ONNX library | Ensure `LD_LIBRARY_PATH=/usr/local/lib` in the container env (it is set in the Dockerfile; this error usually means the image was not rebuilt after a `Dockerfile` change) |
| `model download failed: 403 Forbidden` | GCS model archive removed | Rebuild the image — the model is now baked in via `sentence-transformers-all-MiniLM-L6-v2.tar.gz` (PR-6). If the container was built before this change, run `docker compose build --no-cache mcp`. |
