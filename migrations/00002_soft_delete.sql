-- +goose Up
-- Dedicated partial indexes for active-row queries, separated from 00001_init.sql
-- so this migration can be reviewed and rolled back independently of the schema.
-- All three tables already carry deleted_at from migration 00001; this file adds
-- the lookup-oriented partial indexes and the restore helper view.

-- Partial index for looking up active entities by name (point lookups in open_nodes).
-- The one from 00001 covers the same predicate; this naming convention makes the
-- "soft-delete layer" self-documenting in goose status output.
create index if not exists entities_deleted_at_null_idx on entities (id)
  where deleted_at is null;

-- Partial index for looking up active observations by (entity_id, text) pair
-- (used by the dedup check path in add_observations).
create index if not exists observations_deleted_at_null_idx on observations (id)
  where deleted_at is null;

-- Partial index for looking up active relations (used by delete_relations
-- soft-delete guard and by read_graph traversal).
create index if not exists relations_deleted_at_null_idx on relations (id)
  where deleted_at is null;

-- Restore helper view: simplifies application-side queries that want only
-- active entities without repeating the WHERE clause everywhere.
create or replace view v_active_entities as
  select * from entities where deleted_at is null;

-- +goose Down
drop view if exists v_active_entities;
drop index if exists relations_deleted_at_null_idx;
drop index if exists observations_deleted_at_null_idx;
drop index if exists entities_deleted_at_null_idx;
