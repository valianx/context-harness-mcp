-- +goose Up
-- Phase 2 — project scoping. Adds project_id to nodes and relations,
-- reshapes the UNIQUE constraint on nodes from (name) to (project_id, name),
-- and adds defensive indexes for project-filtered queries.

-- 1. Add project_id columns. NOT NULL DEFAULT 'global' is metadata-only in PG11+
--    (fast-default path); does not rewrite the table. All existing rows
--    backfill to 'global' transparently.
alter table nodes
  add column project_id text not null default 'global';

alter table relations
  add column project_id text not null default 'global';

-- 2. Drop the legacy UNIQUE on nodes(name). The constraint was auto-named
--    `entities_name_key` in 00001_init.sql and was NEVER renamed by the
--    00003 rename migration (Postgres does not auto-rename implicit
--    constraints on ALTER TABLE ... RENAME). Reference the historical name.
alter table nodes drop constraint entities_name_key;

-- 3. Add the new composite UNIQUE on (project_id, name). Two nodes can share
--    a name iff they are in different projects.
alter table nodes
  add constraint nodes_project_name_key unique (project_id, name);

-- 4. Replace the partial active-name index from 00001 (`nodes_name_active_idx`)
--    with one that covers the new access pattern (project + name lookup).
drop index if exists nodes_name_active_idx;
create index nodes_project_name_active_idx
  on nodes (project_id, name)
  where deleted_at is null;

-- 5. Defensive partial index for project-only filter queries
--    (search_nodes/timeline/stats with `WHERE project_id = $1`).
create index nodes_project_id_active_idx
  on nodes (project_id)
  where deleted_at is null;

-- 6. Symmetric defensive index on relations for project-filtered traversal.
create index relations_project_id_active_idx
  on relations (project_id)
  where deleted_at is null;

-- +goose Down
-- Dev/CI only — NEVER run in prod (per docs/knowledge.md [patrón] forward-only).
drop index if exists relations_project_id_active_idx;
drop index if exists nodes_project_id_active_idx;
drop index if exists nodes_project_name_active_idx;
create index nodes_name_active_idx on nodes (name) where deleted_at is null;
alter table nodes drop constraint if exists nodes_project_name_key;
alter table nodes add constraint entities_name_key unique (name);
alter table relations drop column if exists project_id;
alter table nodes    drop column if exists project_id;
