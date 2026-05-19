# Cutover Playbook — ChromaDB → Supabase

This document is the single source of truth for the flag-day migration from
each developer's local ChromaDB knowledge graph to the shared Supabase /
Render deployment of `context-harness-mcp`.

An operator who has this repo cloned and the relevant secrets at hand should
be able to execute the flag-day cutover end-to-end from this document alone.

**Prerequisites:** `gh` CLI authenticated, `psql` reachable,
`docker` reachable (for the emergency fallback), `SUPABASE_DB_URL` exported in
your shell. The `khctl` binary is available inside the Docker image —
invoke it via `docker compose exec mcp khctl <subcommand>` or from the
host if built locally with `go build ./cmd/khctl`.

---

## The 6-Phase Migration Plan (verbatim from `01-architecture.md §Q6`)

> These six phases are the authoritative design. The runbook sections below
> expand each phase with exact commands, expected outputs, and failure
> procedures.

1. **Pre-flight (T-7 days):** Verify Phase 1 has been green for at least one
   full sprint (CI runs against the docker-compose stack; smoke tests pass;
   team members have used the local stack for daily work). Deploy
   `context-harness-mcp` to the Render service URL (Phase 2). Run
   `docker compose exec mcp khctl import <file>` against a recent export.
   Smoke-test the 6 tools from a scratch `~/.claude.json` pointing at the URL.

2. **Freeze (T-0, ~10 min):** Each team member exports their local Chroma via
   `uv run --directory ~/.claude/knowledge-graph python export.py --out /tmp/pre-cutover-$(hostname).json`
   and shares to `claude-dev-team/shared-knowledge/`.

3. **Merge & seed:** Designated operator runs `docker compose exec mcp khctl import`
   on each pre-cutover JSON in chronological order against prod Supabase.
   Idempotent merge: existing node → observations appended with dedup; new
   node → created.

4. **Flag flip:** `claude-dev-team`'s installer is bumped to a version that
   registers MCP `memory` against the Render URL when `KG_BACKEND=supabase`.
   Each developer re-runs `./bin/install.sh` and restarts Claude Code.

5. **Coexistence window (T+0 to T+30 days):** Both backends remain
   installable. `KG_BACKEND=chromadb` keeps the local Chroma DB readable as a
   cold backup. No writes go to Chroma after the flip — orchestrator points at
   remote only.

6. **Sunset (T+30 days):** Default `KG_BACKEND=supabase`; ChromaDB-related
   code in `claude-dev-team/knowledge-graph/` deprecated. Removal in a later
   major bump.

---

## §1 Pre-flight Checklist (T-1 day before flag day)

Run through every item. Do not proceed to flag day if any item fails.

- [ ] Latest weekly `pg_dump` artifact exists and decrypts cleanly:
  ```bash
  # Download from GH Actions artifacts (context-harness-mcp-backups repo or workflow run)
  gpg --decrypt dump.sql.gpg | head -5   # expect PostgreSQL dump header
  ```

- [ ] Render service is healthy (non-200 = service down, abort):
  ```bash
  curl -fsS https://<render-url>/healthz
  # Expected: {"status":"ok","db":"ok"}
  ```

- [ ] Supabase project metrics show <50% of free-tier DB size (500 MB ceiling).
  Check: Supabase dashboard → Project Settings → Database → Usage.

- [ ] GH Actions workflows show green runs over the past 7 days. Verify:
  - `ci.yml` — at least one green run per PR merged.
  - `deploy.yml` — last push-to-main run green.
  - `pg_dump_weekly.yml` — last Sunday run green, artifact available.
  - `supabase_keepalive.yml` — last 6-day run green.

- [ ] `khctl` is reachable via Docker (the binary is baked into the image):
  ```bash
  docker compose exec mcp khctl help   # expect usage output
  ```

- [ ] All team members have been notified of the upcoming cutover window.

---

## §2 Flag-Day Steps

Work through each step in order. Capture the terminal output for the incident
log.

### Step 1 — Announce the cutover window

Notify the team in the project channel before beginning. State the expected
duration (~30 min) and that KG writes should pause during the window.

### Step 2 — Trigger an out-of-band backup (recovery point)

```bash
gh workflow run pg_dump_weekly.yml --repo valianx/context-harness-mcp
```

Wait for the workflow to complete (typically 2-5 min):

```bash
gh run list --workflow=pg_dump_weekly.yml --limit=1
```

Expected: `completed` / `success`. Confirm the artifact appears under the
run before continuing.

### Step 3 — Collect and import all pre-cutover exports

Each team member runs on their own machine (coordinate via the team channel):

```bash
uv run --directory ~/.claude/knowledge-graph python export.py \
  --out /tmp/pre-cutover-$(hostname).json
```

Each member drops their JSON into `claude-dev-team/shared-knowledge/` and
pushes / shares it with the designated operator.

The operator runs `khctl import` once per JSON, in chronological order
(oldest export first). The import accepts both `{"nodes":[...]}` and the
legacy `{"entities":[...]}` shape produced by older ChromaDB exports:

```bash
for f in /path/to/shared-knowledge/pre-cutover-*.json; do
  echo "=== Importing $f ==="
  docker compose exec mcp khctl import "$f"
done
```

Expected output per file (rows will vary):
```
imported nodes=N observations=M relations=K (deduped: nodes=X observations=Y relations=Z)
```

Re-running the same file is safe — deduped counts absorb duplicates.

### Step 4 — Spot-check the import

```bash
docker compose exec mcp khctl export --output /tmp/post-import.json
```

Eyeball the top-level counts:
```bash
python3 -c "
import json
with open('/tmp/post-import.json') as f:
    p = json.load(f)
print('nodes:', p['node_count'], 'relations:', p['relation_count'])
"
```

Expected: `node_count` and `relation_count` are ≥ the sum across all
pre-cutover exports (nodes are deduplicated, so the count may be lower
than a naive sum).

If counts are suspiciously low (e.g., 0 nodes), do NOT proceed — see
§4 Rollback Procedure.

### Step 5 — Smoke-test the Render endpoint

```bash
export MCP_URL=https://<render-url>/mcp

bash scripts/smoke/happy_path.sh
bash scripts/smoke/secret_rejected.sh
bash scripts/smoke/size_rejected.sh
```

All three must print `PASS`. A single `FAIL` output means the server is
not behaving correctly — do NOT proceed with the flag flip.

### Step 6 — Flip team members' `~/.claude.json`

Until `claude-dev-team` PR-9 ships (the `KG_BACKEND` installer switch), each
developer manually updates their `~/.claude.json`:

```json
{
  "mcpServers": {
    "memory": {
      "type": "http",
      "url": "https://<render-url>/mcp"
    }
  }
}
```

Replace the existing `memory` server entry. Restart Claude Code.

Verify the connection is live by running `/memory status` or any `read_graph`
call inside Claude Code.

After PR-9 ships, the flip becomes:
```bash
KG_BACKEND=supabase ./bin/install.sh
```

### Step 7 — Communicate success

Announce in the team channel that the cutover is complete. Include:
- Entity count and relation count from Step 4.
- Render URL team members should be pointing at.
- ChromaDB cold-backup period end date (T+30 days from today).

---

## §3 Rollback Criteria

Initiate rollback if ANY of the following occur within the first 7 days
post-cutover:

- `search_nodes` returns measurably wrong results: >5% missing-result rate
  over 50 representative queries run against known KG content.
- `search_nodes` returns false positives: entities that should not match a
  query appear in the top-3 results consistently across ≥3 different queries.
- Render service is returning non-200 on `/healthz` for more than 5
  consecutive minutes (use GH Actions keepalive alert or manual curl).
- Supabase DB is auto-paused (dashboard shows "paused" status) and cannot be
  resumed within 15 minutes via the dashboard.
- Any `create_nodes` or `add_observations` call returns a DB error
  (not a content-filter rejection) for content that previously worked.
- Weekly `pg_dump` artifact is missing or the decrypt step fails (means
  the backup chain is broken — proceed with rollback before the 7-day
  recovery window closes).

If the service is degraded but recoverable (e.g., Render cold-start spike
causing timeout on first call), wait 3 minutes and retry before declaring a
rollback.

---

## §4 Rollback Procedure

### Step 1 — Flip `~/.claude.json` back to local ChromaDB

Each developer reverts their `~/.claude.json`:

```json
{
  "mcpServers": {
    "memory": {
      "type": "stdio",
      "command": "uv",
      "args": ["run", "--directory", "~/.claude/knowledge-graph", "python", "server.py"]
    }
  }
}
```

Restart Claude Code. Writes resume against local ChromaDB immediately.

### Step 2 — Clean the Supabase tables (optional)

This step is optional because the next import attempt will be idempotent.
Run it only if you want a clean target for the next attempt:

```bash
psql "$SUPABASE_DB_URL" -c "
  TRUNCATE TABLE relations, observations, nodes RESTART IDENTITY CASCADE;
"
```

### Step 3 — Communicate the rollback

Announce in the team channel. Include a one-sentence reason and the
estimated timeline for a re-attempt.

---

## §5 Secret Rotation Runbook

### Rotating `SUPABASE_DB_URL`

The `SUPABASE_DB_URL` contains the Postgres password. Rotate when the
password is compromised or as part of a scheduled rotation.

1. In the Supabase dashboard: **Project Settings → Database → Reset database password**.
   Copy the new password.
2. Construct the new DSN: `postgres://postgres:<new-password>@<host>:5432/postgres`.
3. Update the GitHub secret:
   ```bash
   gh secret set SUPABASE_DB_URL --body "postgres://postgres:<new-password>@..." \
     --repo valianx/context-harness-mcp
   ```
4. Update the Render environment variable: **Render dashboard → Service → Environment →
   `SUPABASE_DB_URL`** → Edit → Save. Render auto-redeploys on env-var change.
5. Verify the next `deploy.yml` run succeeds (goose up + deploy hook).
6. Verify the next `pg_dump_weekly.yml` run produces a valid artifact.

### Rotating `DUMP_PASSPHRASE`

The `DUMP_PASSPHRASE` is the GPG symmetric key used to encrypt weekly dumps.
Rotating it does NOT retroactively re-encrypt existing artifacts — archive any
old encrypted dumps with a note of which passphrase they used before rotating.

1. Generate a new passphrase:
   ```bash
   openssl rand -base64 32
   ```
2. Store old passphrase securely (a password manager entry with the date range
   it covers) so you can still decrypt older artifacts.
3. Update the GitHub secret:
   ```bash
   gh secret set DUMP_PASSPHRASE --body "<new-passphrase>" \
     --repo valianx/context-harness-mcp
   ```
4. Verify the next `pg_dump_weekly.yml` run encrypts with the new passphrase:
   ```bash
   gpg --decrypt --passphrase "<new-passphrase>" --batch dump.sql.gpg | head -3
   ```

### Rotating `RENDER_DEPLOY_HOOK_URL`

The deploy hook URL is a secret URL that triggers a Render deploy.

1. In the Render dashboard: **Service → Settings → Deploy Hook → Rotate**.
   Copy the new URL.
2. Update the GitHub secret:
   ```bash
   gh secret set RENDER_DEPLOY_HOOK_URL --body "https://api.render.com/deploy/..." \
     --repo valianx/context-harness-mcp
   ```
3. Verify the next `deploy.yml` run triggers a Render deploy successfully.

---

## §6 Phase 1 as Emergency Fallback

Invoke this procedure when Supabase Free is auto-paused or quota-exhausted,
Render Free is degraded, or there is a network partition between the team and
either provider.

This brings up a local Postgres + pgvector stack on one developer's machine
and redirects the team to it as a temporary MCP server. The same Docker image
used in production is used here — no code changes needed.

### When to invoke

- Supabase dashboard shows "paused" and cannot be resumed after 15 minutes.
- Render service returns 5xx for >10 consecutive minutes with no deploy in
  flight.
- Both Supabase and Render are unreachable simultaneously (regional outage,
  DNS failure, etc.).

### Steps

1. **Start a local Postgres with pgvector** on one team member's machine
   (the "fallback host"). Use port 5433 to avoid colliding with other local
   Postgres instances:

   ```bash
   docker run -d \
     --name pg-emergency \
     -e POSTGRES_PASSWORD=devpw \
     -p 5433:5432 \
     pgvector/pgvector:pg16
   ```

2. **Apply migrations** against the local Postgres:

   ```bash
   goose -dir migrations postgres \
     "postgres://postgres:devpw@localhost:5433/postgres?sslmode=disable" up
   ```

3. **Download the latest weekly backup artifact** from GitHub Actions:

   ```bash
   gh run download --name pg-dump-encrypted \
     --repo valianx/context-harness-mcp \
     --dir /tmp/backup/
   ```

4. **Decrypt and restore** the backup into the local Postgres:

   ```bash
   gpg --decrypt --passphrase "$DUMP_PASSPHRASE" --batch \
     /tmp/backup/dump.sql.gpg \
     | psql "postgres://postgres:devpw@localhost:5433/postgres"
   ```

5. **Start the MCP server** against the local Postgres:

   ```bash
   SUPABASE_DB_URL="postgres://postgres:devpw@localhost:5433/postgres?sslmode=disable" \
   docker compose up -d
   ```

   The MCP server is now reachable at `http://localhost:7654/mcp` on the
   fallback host.

6. **Redirect team members** to the fallback host's MCP URL. Each developer
   updates their `~/.claude.json`:

   ```json
   {
     "mcpServers": {
       "memory": {
         "type": "http",
         "url": "http://<fallback-host-ip>:7654/mcp"
       }
     }
   }
   ```

   Restart Claude Code. Verify with a `read_graph` call.

7. **Communicate the fallback** in team channels. Include:
   - The fallback host's IP / hostname.
   - The reason for the fallback.
   - An estimated timeline for cloud recovery.

8. **When Supabase / Render are back**, restore the cloud state:

   ```bash
   # Export from the local Postgres (captures writes made during the outage)
   SUPABASE_DB_URL="postgres://postgres:devpw@localhost:5433/postgres?sslmode=disable" \
   docker compose exec mcp khctl export --output /tmp/outage-export.json

   # Import back into production Supabase
   SUPABASE_DB_URL="$SUPABASE_DB_URL" \
   docker compose exec mcp khctl import /tmp/outage-export.json
   ```

   Flip team members back to the Render URL and tear down the local stack:

   ```bash
   docker compose down
   docker rm -f pg-emergency
   ```
