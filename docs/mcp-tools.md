# MCP Tools Reference

The server exposes nine knowledge-graph tools plus a `healthz` probe. Every write tool runs through the [Content Filter](#content-filter--policy-errors) before any database transaction opens — a rejection is observable as an MCP error with a stable `policy/*` code and zero rows written.

JSON wire shapes are byte-for-byte compatible with [`claude-dev-team/knowledge-graph/server.py`](https://github.com/valianx/claude-dev-team/blob/main/knowledge-graph/server.py) so the two backends are interchangeable from the client's point of view.

## Taxonomy

Two closed enums are enforced at both the validator and the DB CHECK constraints:

| `entityType` (9 values) | `relationType` (5 values) |
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

All write tools (1)–(5) wrap their work in a single `pgx.Tx`. If the Content Filter rejects, the transaction is **never opened**. If any insert fails partway through, the transaction rolls back — there are no partial writes. Inserts are deduplicated by DB-level unique constraints (`ON CONFLICT DO NOTHING`).

### 1. `create_entities`

Create new entities with their observations. Idempotent on `name`.

**Arguments**

```json
{
  "entities": [
    {
      "name": "pgvector-hnsw-index",
      "entityType": "pattern",
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
{ "created_entities": 1, "created_observations": 2 }
```

Observations that already exist for the entity (matched by `(entity_id, text)`) are silently deduped and excluded from `created_observations`.

### 2. `add_observations`

Append observations to existing entities. Returns an error if any `entityName` is not found.

**Arguments**

```json
{
  "observations": [
    {
      "entityName": "pgvector-hnsw-index",
      "contents": ["ef_search default 40 is too low for high-recall workloads."]
    }
  ]
}
```

> Note: the array field is **`contents`** (plural), not `content`. This matches the Python reference.

**Success response**

```json
{ "added": 1 }
```

### 3. `delete_entities`

Soft-delete entities by name. Sets `deleted_at = now()`; the row physically remains for audit. Subsequent `read_graph` / `search_nodes` / `open_nodes` calls do not return deleted entities. **No Content Filter** (the argument is a list of names, not user content).

**Arguments**

```json
{ "entityNames": ["pgvector-hnsw-index"] }
```

**Success response**

```json
{ "deleted": 1 }
```

### 4. `delete_observations`

Soft-delete specific observations within an entity. **No Content Filter**.

**Arguments**

```json
{
  "deletions": [
    {
      "entityName": "pgvector-hnsw-index",
      "observations": ["ef_search default 40 is too low for high-recall workloads."]
    }
  ]
}
```

**Success response**

```json
{ "deleted": 1 }
```

### 5. `create_relations`

Create relations between existing entities. Each relation triple `(from, to, relationType)` is unique — re-creating an existing relation is a no-op.

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

### 6. `delete_relations`

Soft-delete relations by triple. **No Content Filter**.

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
{ "deleted": 1 }
```

---

## Read tools

### 7. `search_nodes`

Semantic search over observations. The query string is embedded via `all-MiniLM-L6-v2` (384 dims) and matched against `observations.embedding` using pgvector cosine (`<=>`). Results aggregate per entity (`MIN(distance)`) and are returned with the relations between entities in the result set.

**Arguments**

```json
{ "query": "authentication patterns" }
```

**Success response**

```json
{
  "entities": [
    {
      "name": "oauth-token-rotation",
      "entityType": "pattern",
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

Returns up to 10 entities ordered by cosine distance. If the embedder is unavailable (ONNX init failed, model file missing) the call fails loudly with an MCP error — `search_nodes` never silently degrades to substring matching.

### 8. `open_nodes`

Retrieve specific entities by name plus the relations between them. Names not found are silently skipped (no error).

**Arguments**

```json
{ "names": ["oauth-token-rotation", "supabase-auth"] }
```

**Success response** — same shape as `search_nodes`, restricted to the requested names.

### 9. `read_graph`

Read the entire active knowledge graph. Use sparingly — prefer `search_nodes` or `open_nodes` for targeted queries.

**Arguments**: none.

**Success response**

```json
{
  "entities":  [ /* every active entity */ ],
  "relations": [ /* every active relation */ ],
  "entity_count": 42,
  "relation_count": 117
}
```

---

## Health probe

### `healthz`

Plain HTTP health check. Returned by `GET /healthz` (not over the MCP protocol).

```sh
curl http://localhost:8080/healthz
```

```json
{ "status": "ok", "db": "not-configured" }
```

The `"db"` field reads `"not-configured"` in the current version — a deeper DB ping will land in a follow-up.

---

## Content Filter — policy errors

Write tools (1)–(2)–(5) — those that carry user-provided text — gate every payload through three layers of validation **before** opening any transaction.

### Size limits

Layer 1 (syntactic) enforces two hard size caps:

- **Per-observation cap: 5,000 characters.** A single observation longer than this is almost certainly a pasted file rather than a concise knowledge entry. These caps protect Render Free response timeouts and stay well within Supabase Free row limits.
- **Per-call cap: 50 KB.** The serialised JSON of an entire write call cannot exceed 50 KB. This prevents runaway payloads and multi-entity batches that would exhaust free-tier quotas in a single call.

Violations produce `policy/size-exceeded` with a descriptive Spanish message indicating the limit that was exceeded. No rows are written.

### Secret detection modes

Layer 2 (secrets) scans every observation with [`gitleaks`](https://github.com/gitleaks/gitleaks) (~150 rules) plus 7 inline-regex fallbacks (AWS, GitHub, Anthropic, OpenAI, Stripe, RSA private key, JWT). The detection mode is set via the `SECRET_MODE` environment variable:

- **`reject` (default):** any matched observation aborts the whole call with `policy/secret-detected`. No rows are written.
- **`redact`:** matched secret spans are replaced with `[REDACTED]` in-place before the call proceeds. The rest of the observation text is preserved byte-for-byte. Analogous to OpenTelemetry exporters that scrub PII rather than dropping the span. Useful when agents occasionally log credential-shaped strings that are not real secrets (e.g., example values in documentation).

Any `SECRET_MODE` value other than `reject` or `redact` causes a startup error (fail-fast; no silent fallback).

### Rate limit

To prevent runaway agent loops (e.g., a buggy agent rapidly inserting thousands of observations), write tools are rate-limited **per client IP**:

- **10 writes per 10 seconds** per IP, token-bucket with burst of 10 and refill rate of 1 token/second.
- Applies to `create_entities`, `add_observations`, and `create_relations`. Reads (`search_nodes`, `open_nodes`, `read_graph`) and deletes are unconstrained.
- In HTTP mode, the client IP is read from the leftmost entry of `X-Forwarded-For` (Render passes the real client IP there); falls back to `RemoteAddr` for direct connections.
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
| `policy/taxonomy-violation` | `taxonomy` | `entityType` or `relationType` not in the closed enums, or an observation contains an absolute Windows/WSL/Unix path with a user name, or a `project` entity name is not bare-repo-name. |
| `policy/rate-limited` | `rate-limit` | The client IP has exceeded 10 writes in 10 seconds. |

A policy rejection looks like:

```json
{
  "code": "policy/secret-detected",
  "message": "Observation rejected: AWS access key detected.",
  "layer": "secrets",
  "rejected_observation_index": 1,
  "rejected_entity_index": 2,
  "matched_pattern": "aws-access-key"
}
```

The response carries `IsError: true` at the MCP-protocol level. `message` is in Spanish (the prompt language of the upstream `claude-dev-team` Spanish-first agents); Claude surfaces it directly to the user. `rejected_*` indexes are zero-based into the offending entity/observation; both are `null` when the rejection is not scoped to a single item.

**Atomicity contract**: if the validator returns non-nil, zero rows are written. The DB row counts for `entities`, `observations`, and `relations` are unchanged after a rejected call. This is verified by `tests/tools_test.go` against the golden fixtures in `tests/fixtures/policy_errors/`.

---

## Tool-level invariants

- **Names are unique.** `entities.name` carries a unique constraint. `create_entities` with an already-existing `name` returns the existing entity's id and a `created_entities: 0` in the result; observations are deduped on `(entity_id, text)`.
- **All deletes are soft.** `deleted_at = now()`. No `DELETE FROM` is issued anywhere. Hard deletes are operator-only via Supabase Studio.
- **No partial writes.** Every tool either commits all of its inserts or none.
- **Embeddings are observation-level.** Each `observations` row carries one vector. `search_nodes` aggregates to entity level via `MIN(distance)`.
- **JSON parity with the Python reference.** Field names use camelCase (`entityType`, `relationType`, `entityName`) where the Python server does. Snake_case (`created_entities`, `entity_count`, `rejected_observation_index`) is preserved where it was canonical there too.

For the broader architectural decisions behind these invariants, see [`docs/knowledge.md`](knowledge.md).
