# MCP Tools Reference

The server exposes seventeen tools (sixteen knowledge-graph tools plus one flow-telemetry tool) plus a `healthz` HTTP probe. Every write tool runs through the [Content Filter](#content-filter--policy-errors) before any database transaction opens — a rejection is observable as an MCP error with a stable `policy/*` code and zero rows written.

JSON wire shapes use graph-DB vocabulary (`nodes`, `nodeType`) matching the migration-00003 schema. Field naming conventions are documented per-tool below.

## Taxonomy

Two closed enums are enforced at both the validator and the DB CHECK constraints:

| `nodeType` (9 values) | `relationType` (7 values) |
|---|---|
| `pattern` | `relates_to` |
| `error` | `belongs-to` |
| `constraint` | `calls` |
| `decision` | `uses-stack` |
| `tool-gotcha` | `depends-on` |
| `process-insight` | `supersedes` |
| `project` | `conflicts_with` |
| `service` | |
| `stack-profile` | |

Anything outside these lists is rejected with `policy/taxonomy-violation`.

`supersedes` and `conflicts_with` are **descriptive-only** — neither filters reads automatically. To hide an old node after `mark_superseded(old, new)`, pass `archive_old_observations: true` which soft-deletes the old node's observations (`deleted_at`).

---

## Project scoping

Every node and relation belongs to a **project**, a write-side scope inside a single deployment. The default project is `'global'` — pre-Phase-2 data backfills here transparently and any caller that doesn't pass `project` keeps the existing behavior.

- **Naming**: `^[a-z]([a-z0-9-]{0,62}[a-z0-9])?$` (lowercase letters, digits, dashes; cannot start with digit or dash; cannot end with dash). Validated server-side. Violations return `policy/project-naming-violation`.
- **Writes** (`create_nodes`, `create_relations`) accept an optional `project` field. Default `'global'`. The write is rejected if the project name fails the regex.
- **Reads** (`search_nodes`, `open_nodes`, `read_graph`, `stats`, `timeline`) accept an optional `project` filter. **Omitting `project` returns ALL projects** (back-compat with pre-Phase-2 callers). Filtered reads scope counts, lists, and result sets to the named project.
- **`add_observations` / `update_observations`** derive the project from the parent node — no `project` field needed in the input. Same-name nodes across projects: the handler takes the first homonym alphabetically (caveat documented in §Tool 2).
- **Relations are same-project only**: `create_relations` validates that `from` and `to` share a `project_id`. Cross-project edges return `policy/cross-project-relation` and roll back atomically.
- **Multi-tenant**: this is **not** a tenant isolation mechanism. A single deployment trusts all its users; `project` is a tag for grouping, not for access control. Run separate deployments for separate trust domains.

---

## Write tools

All write tools (1)–(4) wrap their work in a single `pgx.Tx`. If the Content Filter rejects, the transaction is **never opened**. If any step fails partway through, the transaction rolls back — there are no partial writes. Inserts are deduplicated by DB-level unique constraints (`ON CONFLICT DO NOTHING`).

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

### 4. `update_observations`

Atomically replace an existing observation text on a node. The old text is soft-deleted and the new text is inserted with a fresh embedding — all within a single `pgx.Tx`. If any step fails (node not found, observation not found, embedding error), the transaction rolls back completely, leaving the graph unchanged.

**Arguments**

```json
{
  "updates": [
    {
      "nodeName": "pgvector-hnsw-index",
      "old_text": "Defaults m=16, ef_construction=64 suffice for ≤100k vectors.",
      "new_text": "Defaults m=16, ef_construction=64 suffice for ≤500k vectors with recall ≥0.95."
    }
  ]
}
```

- `nodeName` — camelCase, must match an active node exactly.
- `old_text` — exact text of the observation to replace (case-sensitive, no wildcards). Only active observations (not soft-deleted) are matched.
- `new_text` — replacement text. Passes through the Content Filter (size, secrets, taxonomy) before the transaction opens. If `SECRET_MODE=redact`, the redacted version is what gets stored.

**Success response**

```json
{ "updated": 1 }
```

`updated` equals the number of items in `updates` — the tool either succeeds for all or rolls back entirely.

**Error responses**

| Condition | Response |
|---|---|
| `updates` is empty | `IsError: true`, message `"updates must be non-empty"` |
| `old_text` equals `new_text` | `IsError: true`, message `"new_text identical to old_text"` — no Tx opened |
| Node not found | `IsError: true`, message `"node not found: <nodeName>"` — Tx rolled back |
| Observation not found (or already soft-deleted) | `IsError: true`, message `"observation not found: nodeName=<nodeName>"` — Tx rolled back |
| Content Filter violation on `new_text` | `IsError: true`, `policy/*` code (see [Content Filter](#content-filter--policy-errors)) — no Tx opened |
| Rate limit exceeded | `IsError: true`, `policy/rate-limited` |

**Atomicity.** All soft-deletes and inserts run in one `pgx.Tx`. If the N-th update fails, the previous N-1 soft-deletes and inserts are all rolled back. Zero partial writes.

**Attribution.** The new observation row is written with `created_by_user_id` / `created_by_email` from the authenticated request context. The old (soft-deleted) row retains its original attribution as an audit trail.

**Note:** `old_text` is a lookup key only — it does not pass through the Content Filter. Only `new_text` is filtered.

---

## Read tools

### 5. `stats`

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

### 6. `timeline`

List active nodes in reverse-chronological order (newest first). Use this to browse recent knowledge-graph additions or to audit what was added within a time window. Supports optional date bounds and offset-based pagination.

**Arguments** — all optional

```json
{
  "since":  "2026-05-12T00:00:00Z",
  "until":  "2026-05-19T23:59:59Z",
  "limit":  50,
  "offset": 0
}
```

- `since` — RFC3339 timestamp; only nodes created at or after this time are returned. Omit to return from the beginning of recorded time.
- `until` — RFC3339 timestamp; only nodes created at or before this time are returned. Omit to include up to the latest node.
- `limit` — page size; default `50`, max `200`. Values outside `[1, 200]` are silently clamped to the nearest bound.
- `offset` — row offset for pagination; default `0`, max `100000`. Values outside `[0, 100000]` are silently clamped.

**Success response**

```json
{
  "nodes": [
    { "name": "policy-rate-limited", "nodeType": "pattern", "observations": ["..."] }
  ],
  "relations": [
    { "from": "policy-rate-limited", "to": "supabase-auth", "relationType": "relates_to" }
  ],
  "node_count": 1,
  "has_more": false
}
```

- `nodes` is ordered `created_at DESC, id DESC` — newest first, with a stable tiebreak on UUID so pagination never skips or duplicates rows.
- `relations` includes only relations whose **both** endpoints appear in the result `nodes` (same as `open_nodes`).
- `has_more: true` means another page of results exists at `offset + len(nodes)`.
- Soft-deleted nodes are excluded (`deleted_at IS NOT NULL`).
- No rate-limit, no content filter — read-only.

**Pagination example**

```jsonc
// Page 1 — first 2 nodes
{ "limit": 2, "offset": 0 }
// → { "nodes": [N5, N4], "has_more": true }

// Page 2 — next 2 nodes
{ "limit": 2, "offset": 2 }
// → { "nodes": [N3, N2], "has_more": true }

// Page 3 — last node
{ "limit": 2, "offset": 4 }
// → { "nodes": [N1], "has_more": false }
```

**Error responses**

| Condition | Response |
|---|---|
| `since` is not valid RFC3339 | `IsError: true`, message `"invalid since: must be RFC3339"` |
| `until` is not valid RFC3339 | `IsError: true`, message `"invalid until: must be RFC3339"` |
| `since > until` | Empty result set, `has_more: false` — Postgres returns zero rows for an impossible range |

### 7. `doctor`

Run deep operational health probes against every subsystem the server depends on. Use this when you need to diagnose a degraded server or verify that all dependencies are reachable and functional after a deploy.

**Arguments**: none.

**Success response** (all checks pass)

```json
{
  "checks": [
    { "name": "db_ping",            "status": "pass", "duration_ms": 3,   "detail": "" },
    { "name": "pgvector_extension", "status": "pass", "duration_ms": 8,   "detail": "0.7.4" },
    { "name": "embedder",           "status": "pass", "duration_ms": 122, "detail": "all-MiniLM-L6-v2 384 dims" },
    { "name": "gitleaks_detector",  "status": "pass", "duration_ms": 1,   "detail": "150 rules loaded" },
    { "name": "row_counts",         "status": "pass", "duration_ms": 4,   "detail": "nodes=42 obs=137 rel=23" }
  ],
  "degraded": false
}
```

**Degraded response** (one or more checks fail)

```json
{
  "checks": [
    { "name": "db_ping",            "status": "fail", "duration_ms": 5002, "detail": "context deadline exceeded" },
    { "name": "pgvector_extension", "status": "fail", "duration_ms": 0,    "detail": "context deadline exceeded" },
    { "name": "embedder",           "status": "pass", "duration_ms": 145,  "detail": "all-MiniLM-L6-v2 384 dims" },
    { "name": "gitleaks_detector",  "status": "pass", "duration_ms": 1,    "detail": "150 rules loaded" },
    { "name": "row_counts",         "status": "fail", "duration_ms": 5001, "detail": "context deadline exceeded" }
  ],
  "degraded": true
}
```

**Checks (executed sequentially, no short-circuit)**

| # | Name | What it tests | Pass condition |
|---|------|---------------|----------------|
| 1 | `db_ping` | `pool.Ping()` round-trip | No error within 5 s |
| 2 | `pgvector_extension` | `SELECT extversion FROM pg_extension WHERE extname = 'vector'` | Row found within 5 s; `detail` = version string |
| 3 | `embedder` | `embed.Default().Encode(ctx, ["healthcheck"])` | No error AND latency ≤ 200 ms; `detail` = model name + dims |
| 4 | `gitleaks_detector` | Fire the gitleaks `sync.Once` and count rules | Init succeeds; `detail` = rule count |
| 5 | `row_counts` | `SELECT count(*)` on nodes, observations, relations | All 3 queries succeed (counts of 0 are valid on a fresh deploy) |

- Each check has an individual 5-second timeout.
- `degraded: true` when **any** check has `status: "fail"`.
- `doctor` always returns `IsError: false` at the MCP-protocol level. Degradation is in the body, not the envelope — the agent caller must read the `degraded` field.
- No rate-limit, no content filter — read-only.
- **Cold-start note:** the first call after server boot triggers ONNX model initialization (~100–500 ms). Configure container-host healthchecks with a ≥10 s timeout and ≥30 s interval to avoid false-positive failures during startup.

### 8. `search_nodes`

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

### 9. `open_nodes`

Retrieve specific nodes by name plus the relations between them. Names not found are silently skipped (no error).

**Arguments**

```json
{ "names": ["oauth-token-rotation", "supabase-auth"] }
```

**Success response** — same shape as `search_nodes`, restricted to the requested names.

### 10. `read_graph`

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

## Conflict detection

> **Semantic note:** `supersedes` and `conflicts_with` are **descriptive-only** relations. Neither triggers automatic filtering of old or conflicting nodes in `search_nodes`, `open_nodes`, `read_graph`, `timeline`, or `stats`. The old node remains fully visible alongside the new one. **To hide the old node's data after marking, pass `archive_old_observations: true`** to `mark_superseded` — this soft-deletes the old node's observations via `deleted_at`. There is no read-time filter on `supersedes` or `conflicts_with`.

### 11. `find_conflicts`

Semantic similarity search scoped to the same project as the target node. Encodes each of the target node's active observations and queries the pgvector HNSW index for similar nodes — one query per target observation, results aggregated client-side (loop-N-queries strategy). Useful for detecting accidental duplicates or nodes describing overlapping topics.

**Arguments**

```json
{
  "nodeName": "rotacion-claves-oauth",
  "top_k": 5,
  "min_similarity": 0.85,
  "project": "zippy-backoffice"
}
```

- `nodeName` — required. Name of the target node to compare against.
- `top_k` — optional, default `5`, max `50`. Values outside `[1, 50]` are silently clamped.
- `min_similarity` — optional, default `0.85`. Cosine similarity threshold; values outside `[0, 1]` return a validation error.
- `project` — optional. When set, scopes the target node lookup to that project.

**Success response**

```json
{
  "candidates": [
    {
      "name": "token-refresh-strategy",
      "node_type": "pattern",
      "similarity": 0.91,
      "matching_observations_pair": {
        "own_obs": "Rotación de claves OAuth cada 90 días vía Vault.",
        "other_obs": "Token refresh strategy: 90-day rotation via Vault, MiniLM embed."
      }
    }
  ]
}
```

- `candidates` is ordered by `similarity` descending (most similar first).
- `matching_observations_pair.own_obs` is the target node's observation that produced the maximum similarity; `other_obs` is the matching observation from the candidate.
- Returns an empty `candidates` array when the target node has no embeddings or no candidates exceed `min_similarity`.
- Read-only — no rate-limit, no content filter.

**Error responses**

| Condition | Code |
|---|---|
| `nodeName` not found | `policy/node-not-found` |
| `min_similarity` outside `[0, 1]` | `IsError: true`, message `"min_similarity must be in [0, 1]"` |

### 12. `mark_superseded`

Create a `supersedes` relation from the new node to the old node (`new → old`). Optionally soft-delete the old node's active observations so they no longer appear in search results.

> **Direction**: the `supersedes` edge points from `new` to `old`. Semantically: "new supersedes old".
>
> **`reason` field**: logged via `slog.Info` as structured JSON — it is NOT persisted in the database in v0.4.0. Do not include secrets or personal data in `reason`.
>
> **Descriptive-only**: after `mark_superseded`, the old node is still returned by `search_nodes`, `open_nodes`, `read_graph`, `timeline`, and `stats` unless you pass `archive_old_observations: true`. The `supersedes` relation appears as an edge in `read_graph` output.

**Arguments**

```json
{
  "old": "rotacion-claves-oauth",
  "new": "token-refresh-strategy",
  "reason": "The new node describes the same strategy in canonical terms.",
  "archive_old_observations": false,
  "project": "zippy-backoffice"
}
```

- `old` — required. Name of the node being superseded.
- `new` — required. Name of the superseding node.
- `reason` — optional free text (≤500 chars). Logged only — not persisted in DB.
- `archive_old_observations` — optional, default `false`. When `true`, all active observations of the `old` node are soft-deleted (`deleted_at = now()`). This is the only mechanism for hiding old node content — there is no read-time filter on `supersedes`.
- `project` — optional. When set, must match the project of both nodes.

**Success response**

```json
{ "relation_created": true, "observations_archived": 3 }
```

- `observations_archived` is 0 when `archive_old_observations` is false.
- The `supersedes` row carries attribution (`created_by_user_id` / `created_by_email`) from the request context.

**Calling again (idempotency)**

A second call with the same `old` / `new` pair returns `policy/relation-already-exists` — the relation already exists and no change is made.

**Error responses**

| Condition | Code |
|---|---|
| `old` node not found | `policy/node-not-found` |
| `new` node not found | `policy/node-not-found` |
| `old` and `new` in different projects | `policy/cross-project-relation` |
| Relation `supersedes(new → old)` already exists | `policy/relation-already-exists` |
| Rate limit exceeded | `policy/rate-limited` |

---

## Sessions

Sessions bracket a unit of work: `session_start` → ... → `session_end`. Inside a session, any `create_nodes` call can attach the new node to the session via the optional `session_id` field. After the session ends, `session_summary` returns the chronological list of nodes created — including soft-deleted ones, for a complete audit trail.

**Trust model:** sessions are tags, not security boundaries. Any caller can call `session_end` / `session_summary` on any session — same trust model as `project_id` (single-deployment trust).

**Cross-project:** sessions are not scoped to a project. A single session can capture nodes created across multiple projects. The `project_id` field on a session is denormalized at `session_start` time and is informational only.

### `session_id` field on `create_nodes`

Add `session_id` to any node entry in the `create_nodes` array to attach it to an active session:

```json
{
  "nodes": [
    {
      "name": "my-node",
      "nodeType": "pattern",
      "observations": ["..."],
      "session_id": "<uuid from session_start>"
    }
  ]
}
```

- `session_id` is optional. Omitting it leaves `nodes.session_id` as `NULL` (pre-Phase-4 behavior, fully back-compat).
- All nodes in one `create_nodes` call should use the same `session_id` (or no `session_id`). The first non-empty `session_id` found in the batch is used for the entire call.
- If `session_id` is not a valid UUID → `IsError: true`, plain message (no policy code).
- If `session_id` refers to a non-existent session → `policy/session-not-found`.
- If `session_id` refers to a session where `ended_at IS NOT NULL` → `policy/session-already-ended`.

### 14. `session_start`

Start a new session. Returns a `session_id` UUID to pass to subsequent `create_nodes` calls.

**Arguments** — all optional

```json
{
  "project":     "zippy-backoffice",
  "working_dir": "/home/user/zippy"
}
```

- `project` — optional. Must match the project naming regex when provided. Default `'global'`. Stored on the session row as informational context.
- `working_dir` — optional. Free-form path string. Stored verbatim.

**Success response**

```json
{
  "session_id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
  "started_at": "2026-05-20T14:00:00Z"
}
```

**Error responses**

| Condition | Code |
|---|---|
| `project` fails naming regex | `policy/project-naming-violation` |
| Rate limit exceeded | `policy/rate-limited` |

### 15. `session_end`

End a session. Idempotent — calling it on an already-ended session returns the same row unchanged.

**Arguments**

```json
{
  "session_id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
  "summary":    "Learned about retry strategies and circuit-breaker patterns."
}
```

- `session_id` — required. UUID returned by `session_start`.
- `summary` — optional. Free-form text (≤4096 chars). Persisted in `sessions.summary`. On a repeated call the summary is NOT overwritten — the first `session_end` wins.

**Success response**

```json
{
  "session_id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
  "ended_at":   "2026-05-20T14:30:00Z",
  "node_count": 5
}
```

- `node_count` — total number of nodes (including soft-deleted) attached to this session.
- On a repeated call `ended_at` equals the original end timestamp.

**Error responses**

| Condition | Code |
|---|---|
| `session_id` not a valid UUID | `IsError: true`, plain message |
| `session_id` not found | `policy/session-not-found` |
| `summary` exceeds 4096 chars | `IsError: true`, plain message |
| Rate limit exceeded | `policy/rate-limited` |

### 16. `session_summary`

Return the full list of nodes created during a session, ordered chronologically. Includes soft-deleted nodes — this is a full audit trail, not a filtered view.

**Arguments**

```json
{ "session_id": "3fa85f64-5717-4562-b3fc-2c963f66afa6" }
```

**Success response**

```json
{
  "session_id":  "3fa85f64-5717-4562-b3fc-2c963f66afa6",
  "started_at":  "2026-05-20T14:00:00Z",
  "ended_at":    "2026-05-20T14:30:00Z",
  "project_id":  "zippy-backoffice",
  "working_dir": "/home/user/zippy",
  "summary":     "Learned about retry strategies.",
  "nodes": [
    {
      "id":                "uuid",
      "name":              "retry-policy",
      "node_type":         "pattern",
      "project_id":        "zippy-backoffice",
      "created_at":        "2026-05-20T14:05:00Z",
      "deleted_at":        null,
      "observation_count": 3
    }
  ]
}
```

- `nodes` is ordered by `created_at ASC` — chronological creation order.
- `deleted_at` is non-null for soft-deleted nodes (they are included intentionally).
- `observation_count` counts **active** observations only (`deleted_at IS NULL`).
- `ended_at` is `null` when the session is still active.
- `working_dir` and `summary` are `null` when not set.
- Read-only — no rate-limit, no content filter.

**Error responses**

| Condition | Code |
|---|---|
| `session_id` not a valid UUID | `IsError: true`, plain message |
| `session_id` not found | `policy/session-not-found` |

---

## Semantic classification

### 13. `suggest_node_type`

Returns the top-3 most likely `nodeType` values for a free-form text, ranked by cosine similarity to per-type centroids computed from all active observations. Read-only — no rate limit, no content filter.

**How it works:** for each `nodeType` that has at least one active observation with a non-null embedding, the tool computes the element-wise mean of all those observation vectors (the centroid). The input text is embedded with the same model (`all-MiniLM-L6-v2`). Cosine similarity is computed between the query vector and each centroid; the top-3 are returned sorted by score descending.

**Arguments**

```json
{
  "text": "How do I handle session timeouts gracefully?",
  "project": "foo"
}
```

- `text` — required. Free-form text to classify. Max 8192 characters. Whitespace-trimmed.
- `project` — optional. Scope the centroid computation to one project. Omitting it spans all projects.

**Success response**

```json
{
  "suggestions": [
    {"node_type": "pattern", "score": 0.78},
    {"node_type": "decision", "score": 0.62},
    {"node_type": "constraint", "score": 0.41}
  ],
  "stats": {
    "centroids_computed": 5,
    "types_unseen": ["service", "stack-profile", "project", "tool-gotcha"]
  }
}
```

- `score` is cosine similarity in `[-1, 1]`; higher = closer match.
- `centroids_computed` = number of nodeType values that had at least one active observation to average.
- `types_unseen` = nodeType values that had no active observations (no centroid available).
- Always ≤ 3 suggestions (capped at `min(3, centroids_computed)`).

**Empty corpus**

When no active observations with embeddings exist, the tool returns successfully (not an error):

```json
{
  "suggestions": [],
  "stats": {"centroids_computed": 0, "types_unseen": ["constraint", "decision", ...all 9...]}
}
```

**Error responses**

| Condition | Result |
|---|---|
| `text` is empty or whitespace-only | MCP error |
| `text` exceeds 8192 characters | MCP error |
| `project` fails naming regex | `policy/project-naming-violation` |

---

---

## Flow telemetry

### 17. `record_flow_event`

Record a metadata-only pipeline friction event. The tool validates the payload against the closed flow-event schema, runs it through the Content Filter (secrets + user-path layers), and relays a scrubbed structured log line to the observability backend (Axiom) when `CH_OBSERVABILITY_ENABLED=true`. The event is **never persisted to Postgres** — it is a fire-and-forget telemetry signal only.

**Prerequisite:** `CH_OBSERVABILITY_ENABLED=true` and a configured OTLP endpoint. When observability is disabled the tool still validates and returns `{"recorded": true}` — the relay is silently skipped.

**Arguments**

```json
{
  "event": {
    "event":      "gate.fail",
    "ts":         "2026-06-21T10:00:00Z",
    "project":    "team-harness",
    "task_type":  "feature",
    "th_version": "2.117.2",
    "gate":       "STAGE-GATE-2",
    "verdict":    "fail"
  }
}
```

**Common fields (every event)**

| Field | Type | Required | Constraint |
|-------|------|----------|------------|
| `event` | string | yes | Closed 8-value enum (see below) |
| `ts` | string | yes | RFC3339 UTC |
| `project` | string | yes | Bare repo name — same regex as other write tools (`^[a-z]([a-z0-9-]{0,62}[a-z0-9])?$`) |
| `task_type` | string | yes | `feature \| fix \| hotfix \| refactor \| enhancement \| docs \| research` |
| `th_version` | string | yes | Plugin semver string (e.g. `2.117.2`) |

**Closed `event` enum + per-event fields**

| `event` | per-event fields | field constraints |
|---------|------------------|-------------------|
| `guard.block` | `hook`, `reason`, `resolved` | `hook` ∈ {prepublish, dev, policy}; `reason` ∈ {over-bump, secret, outward}; `resolved` bool |
| `gate.fail` | `gate`, `verdict` | `gate` ∈ {STAGE-GATE-1, STAGE-GATE-2, STAGE-GATE-3, acceptance, plan-review}; `verdict` ∈ {fail, concerns} |
| `verify.reject` | `agent`, `verdict` | `agent` ∈ {qa, security, tester, acceptance}; `verdict` ∈ {fail, concerns} |
| `iteration.loop` | `stage`, `iterations` | `stage` ∈ {1, 2, 3}; `iterations` int ≥ 2 |
| `blocked` | `reason` | `reason` ∈ {no-dispatch, manual-push, guard, dependency} |
| `scope.collapse` | `items_dropped` | `items_dropped` int ≥ 1 |
| `mcp.unavailable` | `op` | `op` ∈ {read, write} |
| `abandon` | `last_stage` | `last_stage` ∈ {1, 2, 3} |

**Success response**

```json
{ "recorded": true }
```

**Error responses**

| Condition | Code |
|-----------|------|
| `event` not in the closed 8-value enum | `policy/flow-event-unknown-type` |
| A per-event field is outside its closed sub-enum or numeric boundary | `policy/flow-event-bad-field` |
| Any string field contains a secret | `policy/secret-detected` |
| Any string field contains an absolute user-path | `policy/taxonomy-violation` |
| `project` fails the naming regex | `policy/project-naming-violation` |
| Rate limit exceeded | `policy/rate-limited` |

**Policy notes**

- Flow events are **metadata-only** — no diff, no code, no file paths, no commit messages. Every field is a bounded enum, int, bool, semver, or timestamp.
- The Content Filter's secrets + user-path layers run on all string fields before relay.
- The relay path is a scrubbed structured slog log line (`signal="flow_event"`) — **never** a metric (metric labels are unscrubbed by SDK design).
- A closed-enum rejection for `event` also emits a `flow_event_rejected` log (`reason=unknown-type`, offending token) so drifted clients surface in Axiom.
- Rate-limited at the same per-IP/per-sub budget as all other write tools.
- No `pgx.Tx` is opened — the tool does no database writes.

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

### `GET /healthz`

Plain HTTP health check consumed by container-host healthcheck daemons (Railway, Render, Fly, Docker compose). Runs the same 5 deep probes as the `doctor` MCP tool via a shared `healthz.Run` implementation.

```sh
curl -i http://localhost:7654/healthz
```

**Healthy response (HTTP 200)**

```json
{
  "checks": [
    { "name": "db_ping",            "status": "pass", "duration_ms": 3,   "detail": "" },
    { "name": "pgvector_extension", "status": "pass", "duration_ms": 8,   "detail": "0.7.4" },
    { "name": "embedder",           "status": "pass", "duration_ms": 122, "detail": "all-MiniLM-L6-v2 384 dims" },
    { "name": "gitleaks_detector",  "status": "pass", "duration_ms": 1,   "detail": "150 rules loaded" },
    { "name": "row_counts",         "status": "pass", "duration_ms": 4,   "detail": "nodes=42 obs=137 rel=23" }
  ],
  "degraded": false
}
```

**Degraded response (HTTP 503)**

```json
{
  "checks": [
    { "name": "db_ping", "status": "fail", "duration_ms": 5002, "detail": "context deadline exceeded" },
    ...
  ],
  "degraded": true
}
```

| HTTP status | Meaning |
|---|---|
| `200 OK` | All 5 checks passed — server is healthy |
| `503 Service Unavailable` | One or more checks failed — server is degraded |

**Breaking change from v0.2.x:** the previous `/healthz` always returned HTTP 200 with `{"status":"ok","db":"not-configured"}`. Operators who relied on the always-200 behavior or parsed the `status`/`db` fields must update their healthcheck to use the HTTP status code (200/503) and the new `checks[]` / `degraded` body shape. Container hosts that simply check for a non-5xx response will automatically benefit from the 503 signal.

**Cold-start note:** the first request after server boot triggers ONNX model initialization (100–500 ms). Configure container healthchecks with `timeout ≥ 10s` and `interval ≥ 30s` to avoid false-positive unhealthy signals during startup.

---

## Content Filter — policy errors

Write tools (1)–(2)–(3)–(4) — those that carry user-provided text — gate every payload through three layers of validation **before** opening any transaction.

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
- Applies to `create_nodes`, `add_observations`, `create_relations`, and `update_observations`. Reads (`search_nodes`, `open_nodes`, `read_graph`, `stats`, `timeline`, `doctor`) are unconstrained.
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
| `policy/project-naming-violation` | `project` | The `project` field fails the regex `^[a-z]([a-z0-9-]{0,62}[a-z0-9])?$`. |
| `policy/cross-project-relation` | `project` | A relation is attempted between nodes in different projects, or the explicit `project` hint does not match the nodes' actual project. |
| `policy/node-not-found` | `project` | A node name provided to `find_conflicts` or `mark_superseded` does not match any active node. |
| `policy/relation-already-exists` | `project` | `mark_superseded` is called for a `(new, old)` pair that already has a `supersedes` relation. |
| `policy/session-not-found` | `session` | A `session_id` provided to `session_end`, `session_summary`, or `create_nodes` does not match any row in `sessions`. |
| `policy/session-already-ended` | `session` | `create_nodes` is called with a `session_id` whose `ended_at IS NOT NULL`. |

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
