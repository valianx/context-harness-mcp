# MCP Tools Reference

The server exposes seven knowledge-graph tools plus a `healthz` probe. Every write tool runs through the [Content Filter](#content-filter--policy-errors) before any database transaction opens — a rejection is observable as an MCP error with a stable `policy/*` code and zero rows written.

JSON wire shapes use graph-DB vocabulary (`nodes`, `nodeType`) matching the migration-00003 schema. Field naming conventions are documented per-tool below.

## Taxonomy

Two closed enums are enforced at both the validator and the DB CHECK constraints:

| `nodeType` (9 values) | `relationType` (5 values) |
|---|---|
| `pattern` | `relates_to` |
| `error` | `belongs-to` |
| `constraint` | `calls` |
| `decision` | `uses-stack` |
| `tool-gotcha` | `depends-on` |
| `process-insight` | |
| `project` | |
| `service` | |
| `stack-profile` | |

Anything outside these lists is rejected with `policy/taxonomy-violation`.

---

## Write tools

All write tools (1)–(3) wrap their work in a single `pgx.Tx`. If the Content Filter rejects, the transaction is **never opened**. If any insert fails partway through, the transaction rolls back — there are no partial writes. Inserts are deduplicated by DB-level unique constraints (`ON CONFLICT DO NOTHING`).

### 1. `create_nodes`

Create new nodes with their observations. Idempotent on `name`.

**Arguments**

```json
{
  "nodes": [
    {
      "name": "pgvector-hnsw-index",
      "nodeType": "pattern",
      "observations": [
        "HNSW index on vector(384) columns with cosine ops.",
        "Defaults m=16, ef_construction=64 suffice for ≤100k vectors."
      ]
    }
  ]
}
```

**Success response**

```json
{ "created_nodes": 1, "created_observations": 2 }
```

Observations that already exist for the node (matched by `(node_id, text)`) are silently deduped and excluded from `created_observations`.

### 2. `add_observations`

Append observations to existing nodes. Returns an error if any `nodeName` is not found.

**Arguments**

```json
{
  "observations": [
    {
      "nodeName": "pgvector-hnsw-index",
      "contents": ["ef_search default 40 is too low for high-recall workloads."]
    }
  ]
}
```

> Note: the array field is **`contents`** (plural), not `content`.

**Success response**

```json
{ "added": 1 }
```

### 3. `create_relations`

Create relations between existing nodes. Each relation triple `(from, to, relationType)` is unique — re-creating an existing relation is a no-op.

**Arguments**

```json
{
  "relations": [
    { "from": "pgvector-hnsw-index", "to": "supabase-postgres", "relationType": "belongs-to" }
  ]
}
```

**Success response**

```json
{ "created": 1 }
```

---

## Read tools

### 4. `stats`

Return aggregated counts for the active knowledge graph. Use this instead of `read_graph` when you only need counts — it is significantly cheaper because it runs a single aggregated SQL query rather than fetching every node, observation, and relation.

**Arguments**: none.

**Success response**

```json
{
  "node_count": 42,
  "observation_count": 137,
  "relation_count": 23,
  "by_type": {
    "pattern": 18,
    "error": 5,
    "constraint": 4,
    "decision": 7,
    "tool-gotcha": 3,
    "process-insight": 2,
    "project": 1,
    "service": 1,
    "stack-profile": 1
  },
  "oldest_node": { "name": "supabase-postgres", "created_at": "2026-04-01T10:14:22Z" },
  "newest_node": { "name": "policy-rate-limited", "created_at": "2026-05-19T09:30:00Z" }
}
```

- `by_type` only includes `nodeType` values with at least one active node — types with zero active nodes are omitted (callers can infer the missing types are at zero).
- `oldest_node` / `newest_node` are JSON `null` when `node_count == 0` (empty graph).
- Soft-deleted nodes (`deleted_at IS NOT NULL`) are excluded from all counts.
- No rate-limit, no content filter — read-only.

### 5. `search_nodes`

Semantic search over observations. The query string is embedded via `all-MiniLM-L6-v2` (384 dims) and matched against `observations.embedding` using pgvector cosine (`<=>`). Results aggregate per node (`MIN(distance)`) and are returned with the relations between nodes in the result set.

**Arguments**

```json
{ "query": "authentication patterns" }
```

**Success response**

```json
{
  "nodes": [
    {
      "name": "oauth-token-rotation",
      "nodeType": "pattern",
      "observations": [
        "Refresh tokens rotate every 30 days.",
        "JWT signing keys rotate via JWKS."
      ]
    }
  ],
  "relations": [
    { "from": "oauth-token-rotation", "to": "supabase-auth", "relationType": "calls" }
  ]
}
```

Returns up to 10 nodes ordered by cosine distance. If the embedder is unavailable (ONNX init failed, model file missing) the call fails loudly with an MCP error — `search_nodes` never silently degrades to substring matching.

### 6. `open_nodes`

Retrieve specific nodes by name plus the relations between them. Names not found are silently skipped (no error).

**Arguments**

```json
{ "names": ["oauth-token-rotation", "supabase-auth"] }
```

**Success response** — same shape as `search_nodes`, restricted to the requested names.

### 7. `read_graph`

Read the entire active knowledge graph. Use sparingly — prefer `search_nodes` or `open_nodes` for targeted queries.

**Arguments**: none.

**Success response**

```json
{
  "nodes":          [ /* every active node */ ],
  "relations":      [ /* every active relation */ ],
  "node_count":     42,
  "relation_count": 117
}
```

---

## Administrative deletions

Soft-delete operations (`delete_nodes`, `delete_observations`, `delete_relations`) are **not** exposed as MCP tools. Exposing destructive operations on an unauthenticated public endpoint would allow any caller to irreversibly soft-delete graph content without access controls.

Deletions are available as store-level functions for operator use only:

```sql
-- Soft-delete a node (and its observations cascade via DB trigger):
UPDATE nodes SET deleted_at = now() WHERE name = 'my-node-name';

-- Soft-delete a specific observation:
UPDATE observations SET deleted_at = now()
WHERE node_id = (SELECT id FROM nodes WHERE name = 'my-node-name')
  AND text = 'the observation text to remove';

-- Soft-delete a relation triple:
UPDATE relations SET deleted_at = now()
WHERE from_node_id = (SELECT id FROM nodes WHERE name = 'from-node')
  AND to_node_id   = (SELECT id FROM nodes WHERE name = 'to-node')
  AND relation_type = 'calls';
```

Hard deletes are operator-only via Supabase Studio. All rows retain `deleted_at` for audit.

---

## Health probe

### `healthz`

Plain HTTP health check. Returned by `GET /healthz` (not over the MCP protocol).

```sh
curl http://localhost:7654/healthz
```

```json
{ "status": "ok", "db": "not-configured" }
```

The `"db"` field reads `"not-configured"` in the current version — a deeper DB ping will land in a follow-up.

---

## Content Filter — policy errors

Write tools (1)–(2)–(3) — those that carry user-provided text — gate every payload through three layers of validation **before** opening any transaction.

### Size limits

Layer 1 (syntactic) enforces two hard size caps:

- **Per-observation cap: 5,000 characters.** A single observation longer than this is almost certainly a pasted file rather than a concise knowledge entry. These caps protect free-tier container hosts from response timeouts and stay well within typical managed-Postgres row limits.
- **Per-call cap: 50 KB.** The serialised JSON of an entire write call cannot exceed 50 KB. This prevents runaway payloads and multi-node batches that would exhaust free-tier quotas in a single call.

Violations produce `policy/size-exceeded` with a descriptive Spanish message indicating the limit that was exceeded. No rows are written.

### Secret detection modes

Layer 2 (secrets) scans every observation with [`gitleaks`](https://github.com/gitleaks/gitleaks) (~150 rules) plus 7 inline-regex fallbacks (AWS, GitHub, Anthropic, OpenAI, Stripe, RSA private key, JWT). The detection mode is set via the `SECRET_MODE` environment variable:

- **`reject` (default):** any matched observation aborts the whole call with `policy/secret-detected`. No rows are written.
- **`redact`:** matched secret spans are replaced with `[REDACTED]` in-place before the call proceeds. The rest of the observation text is preserved byte-for-byte. Analogous to OpenTelemetry exporters that scrub PII rather than dropping the span. Useful when agents occasionally log credential-shaped strings that are not real secrets (e.g., example values in documentation).

Any `SECRET_MODE` value other than `reject` or `redact` causes a startup error (fail-fast; no silent fallback).

### Rate limit

To prevent runaway agent loops (e.g., a buggy agent rapidly inserting thousands of observations), write tools are rate-limited **per client IP**:

- **10 writes per 10 seconds** per IP, token-bucket with burst of 10 and refill rate of 1 token/second.
- Applies to `create_nodes`, `add_observations`, and `create_relations`. Reads (`search_nodes`, `open_nodes`, `read_graph`) are unconstrained.
- In HTTP mode, the client IP is read from the leftmost entry of `X-Forwarded-For` (most container hosts forward the real client IP there); falls back to `RemoteAddr` for direct connections.
- In stdio mode (local Claude Code), rate limiting is skipped — no IP is available.

A rate-limited call returns `policy/rate-limited` with a `retry_after_seconds` field:

```json
{
  "code": "policy/rate-limited",
  "message": "Demasiadas escrituras desde esta IP. Reintentar en 2 segundos.",
  "layer": "rate-limit",
  "retry_after_seconds": 2
}
```

### Policy error codes

| Code | Layer | Triggers when |
|---|---|---|
| `policy/size-exceeded` | `syntactic` | Any observation exceeds 5,000 chars **or** the total request body exceeds 50 KB. |
| `policy/junk-pattern` | `syntactic` | An observation matches the curated junk-pattern denylist (`internal/validate/denylist.go`). |
| `policy/secret-detected` | `secrets` | A secret is detected and `SECRET_MODE=reject` (default). |
| `policy/taxonomy-violation` | `taxonomy` | `nodeType` or `relationType` not in the closed enums, or an observation contains an absolute Windows/WSL/Unix path with a user name, or a `project` node name is not bare-repo-name. |
| `policy/rate-limited` | `rate-limit` | The client IP has exceeded 10 writes in 10 seconds. |

A policy rejection looks like:

```json
{
  "code": "policy/secret-detected",
  "message": "Observation rejected: AWS access key detected.",
  "layer": "secrets",
  "rejected_observation_index": 1,
  "rejected_node_index": 2,
  "matched_pattern": "aws-access-key"
}
```

The response carries `IsError: true` at the MCP-protocol level. `message` is in Spanish (the operator's chosen surface language); Claude surfaces it directly to the user. `rejected_*` indexes are zero-based into the offending node/observation; both are `null` when the rejection is not scoped to a single item.

**Atomicity contract**: if the validator returns non-nil, zero rows are written. The DB row counts for `nodes`, `observations`, and `relations` are unchanged after a rejected call. This is verified by `tests/tools_test.go` against the golden fixtures in `tests/fixtures/policy_errors/`.

---

## Web viewer

`GET /viewer/` serves a public single-page HTML browser of the knowledge graph. It is embedded directly in the Go binary via `go:embed` — no separate process, no additional port, no external assets. The page loads all active nodes on first render and supports semantic search via a debounced input box (250 ms). Search is powered by the same `embed.Default` + pgvector cosine path used by the `search_nodes` MCP tool; an empty query lists all nodes ordered by creation date.

Access is unauthenticated — the same exposure level as the `read_graph` and `search_nodes` MCP tools. No write or delete operations are exposed through the viewer. Useful for operators inspecting the graph without writing SQL or calling the MCP protocol directly.

---

## Tool-level invariants

- **Names are unique.** `nodes.name` carries a unique constraint. `create_nodes` with an already-existing `name` returns the existing node's id and a `created_nodes: 0` in the result; observations are deduped on `(node_id, text)`.
- **All deletes are soft.** `deleted_at = now()`. No `DELETE FROM` is issued anywhere. Hard deletes are operator-only via Supabase Studio.
- **No partial writes.** Every tool either commits all of its inserts or none.
- **Embeddings are observation-level.** Each `observations` row carries one vector. `search_nodes` aggregates to node level via `MIN(distance)`.
- **Stable JSON field naming.** camelCase for input fields (`nodeType`, `relationType`, `nodeName`), snake_case for response counters (`created_nodes`, `node_count`, `rejected_observation_index`). Adding fields is non-breaking; renaming or removing is a breaking change.

For the broader architectural decisions behind these invariants, see [`docs/knowledge.md`](knowledge.md).
