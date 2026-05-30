# Roadmap — context-harness-mcp

> Plan de evolución de `context-harness-mcp` de tool personal a **memoria compartida de equipo**.
>
> Detalle operativo y AC por phase: [`session-docs/v0.2.0-team-features/00-roadmap.md`](../session-docs/v0.2.0-team-features/00-roadmap.md).

---

## Visión

`context-harness-mcp` es un **producto open source**: cualquier team puede levantar su propia instancia privada en cualquier hosting de containers (Railway / Render / Fly / Coolify / self-hosted / etc.) contra cualquier Postgres+pgvector (Supabase, Neon, RDS, propio, …).

Corre como memoria compartida de equipo con auth Supabase+JWT, project scoping, conflict detection, sessions, y observabilidad Axiom. Cualquier dev del team que deployó la instancia puede leer/escribir al mismo grafo con su identidad atribuida, con revocación instantánea, y con herramientas que evitan la entropía típica de un KG compartido (conflictos, supersedes, project scoping, sessions). Las seis phases del roadmap original están entregadas a `main` como versión `0.5.0`.

Cada instancia es **single-tenant** (un team = un deploy). Multi-team = múltiples instancias independientes, cada una con su propio Supabase y su propio `MCP_JWT_SECRET`.

Comparativa que motivó el plan: **engram** (Gentleman-Programming/engram) tiene 19 tools y captura local-first. Reusamos sus mejores ideas — `update_observations`, conflict detection, sessions, `doctor`, `stats`, `timeline` — adaptadas al modelo cloud-shared que ya tenemos, con la ventaja diferenciadora de **búsqueda semántica real vía pgvector** (engram usa FTS5 léxico).

---

## Decisiones locked

| Tema | Decisión |
|---|---|
| **Auth provider** | Supabase Auth (mismo proyecto donde ya corre la DB; cero vendor nuevo). |
| **Invitación de users** | Admin desde dashboard de Supabase (no CLI). |
| **Login del dev** | Magic link → callback page en el server → setear password → recibir snippet de `~/.claude.json`. |
| **Token bearer** | MCP server emite su propio JWT HS256, expiry **1 año**. Paste once, forever. |
| **Revocación** | Webhook de Supabase → tabla local `users(revoked_at)` chequeada por middleware en cada request (cache 30s). Admin borra/banea en Supabase → <2s. |
| **Multi-proyecto** | Columna opcional `project_id` con default `'global'` (aditivo, sin breaking). |
| **Conflict detection** | Tools nuevos `find_conflicts` + `mark_superseded` + extensión del enum `relationType` con `supersedes` / `conflicts_with`. |
| **Sessions** | Tabla `sessions` + tools `session_start/end/summary`; columna `session_id` nullable en writes. |

---

## Phases

### Phase 0 — Seguridad para uso en equipo (~4 días)

Bloquea todo lo demás. Sin auth no podemos compartir el server.

- Supabase Auth habilitado en el proyecto existente.
- Página de callback HTML embebida en el server (`/auth/callback`, `/auth/login`).
- JWT propio del MCP server (HS256, 1y).
- Tabla `users` + webhook desde Supabase para revocación instantánea.
- Atribución `created_by_user_id` / `created_by_email` en `nodes` / `observations` / `relations`.
- Rate limit pasa de "por IP" a "por `sub` claim".

### Phase 1 — Quick wins (~2 días)

Corrige el "el KG sólo crece y nunca se corrige".

- `update_observations` tool — reemplaza una observación vieja por una nueva (soft-delete + insert nuevo en una tx).
- `stats` tool — counts agregados en server-side (en lugar de bajar todo el grafo con `read_graph`).
- `timeline` tool — listar nodos por `created_at` con paginación.
- `doctor` tool — health enriquecido (DB ping, pgvector, ONNX sanity, gitleaks init).

### Phase 2 — Project scoping (~2 días)

Habilita usar el mismo server desde múltiples repos sin contaminación cruzada.

- Migration aditiva: `project_id text NOT NULL DEFAULT 'global'` en `nodes` y `relations`.
- Unique constraint `(project_id, name)` reemplaza el actual sobre `name`.
- Tools de write aceptan `project` opcional; reads aceptan filtro `project` opcional (default = todos).
- `khctl export/import --project <name>`.
- Viewer: dropdown de proyecto + chip en cada nodo.

### Phase 3 — Conflict detection (~3 días)

El **diferenciador** vs engram. Su FTS5 sólo encuentra conflictos léxicos; nuestro pgvector los encuentra semánticos.

- Extender `relationType` enum con `supersedes` y `conflicts_with`.
- Tool `find_conflicts(nodeName, top_k, min_similarity)` — devuelve top-K nodos del mismo project con observaciones semánticamente parecidas (cosine via pgvector).
- Tool `mark_superseded(old, new, reason)` — crea relación + opt-in archive de observaciones del viejo.
- Hook en `team-harness` (skill `memory.md`): `check-conflicts <name>` invoca el tool y propone al agente decidir `supersedes` / `conflicts_with` / `relates_to`.

### Phase 4 — Sessions + passive capture (~2 días)

- Tabla `sessions(id, user_id, project_id, working_dir, started_at, ended_at, summary)`.
- Columna `session_id` nullable en `nodes`/`observations`.
- Tools `session_start` / `session_end` / `session_summary`.
- Hook en `team-harness` (PostToolUse en `delivery` agent): captura pasiva al cerrar una task con AC validados → emite `create_nodes` con un `process-insight` describiendo qué se aprendió.

### Phase 5 — Polish operacional (~3-4 días)

Nice-to-have, no bloqueante.

- `suggest_node_type(text)` — clasificador por centroide de embeddings.
- Viewer con filtros (project, type) + visualización de relaciones `supersedes`.
- `khctl backup` / `khctl restore` (wrappers del `pg_dump` semanal).
- Metrics endpoint `/metrics` Prometheus (writes/s, search latency p50/p95, content-filter rejects por código).

---

## Cronograma estimado

| Phase | Esfuerzo | Bloquea |
|---|---|---|
| 0 — Seguridad | ~4 días | Todas las demás |
| 1 — Quick wins | ~2 días | — (paralelizable con 2) |
| 2 — Project scoping | ~2 días | Precede a 3 (find_conflicts filtra por project) |
| 3 — Conflicts | ~3 días | — |
| 4 — Sessions | ~2 días | — |
| 5 — Polish | ~3-4 días | — |

**Total sin paralelizar:** ~3-4 semanas dev full-time. Un primer release defendible (`v0.2.0`) cubre Phase 0 + Phase 1.

---

## Status

| Phase | Status |
|---|---|
| 0 — Seguridad | ✅ Shipped |
| 1 — Quick wins | ✅ Shipped |
| 2 — Project scoping | ✅ Shipped |
| 3 — Conflicts | ✅ Shipped |
| 4 — Sessions | ✅ Shipped |
| 5 — Polish | ✅ Shipped |

Última actualización: 2026-05-29.
