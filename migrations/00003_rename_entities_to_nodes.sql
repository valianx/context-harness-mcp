-- +goose Up
-- Rename the entities table and its columns to use graph-DB vocabulary (nodes).
-- This is a pure rename migration — no data is modified, no rows are deleted.
-- The soft-delete columns (deleted_at) and all row data survive intact.

-- Rename column entity_type → node_type on the entities table.
alter table entities rename column entity_type to node_type;

-- Rename the entities table itself to nodes.
alter table entities rename to nodes;

-- Rename the foreign-key column entity_id → node_id on observations.
alter table observations rename column entity_id to node_id;

-- Rename the FK columns on relations.
alter table relations rename column from_entity_id to from_node_id;
alter table relations rename column to_entity_id   to to_node_id;

-- Rename partial indexes from 00001_init.sql.
alter index entities_entity_type_active_idx rename to nodes_node_type_active_idx;
alter index entities_name_active_idx         rename to nodes_name_active_idx;
alter index observations_entity_id_active_idx rename to observations_node_id_active_idx;
-- relations_from_active_idx and relations_to_active_idx are already neutral names;
-- they reference the table but the column names in the predicate do not appear in
-- the index name, so no rename is needed.

-- Rename soft-delete helper indexes from 00002_soft_delete.sql.
alter index entities_deleted_at_null_idx rename to nodes_deleted_at_null_idx;

-- Rename the restore helper view from 00002_soft_delete.sql.
alter view v_active_entities rename to v_active_nodes;

-- +goose Down
-- Inverse of the Up block. goose Down is for dev/CI only — never run in prod.

alter view v_active_nodes rename to v_active_entities;

alter index nodes_deleted_at_null_idx rename to entities_deleted_at_null_idx;

alter index observations_node_id_active_idx rename to observations_entity_id_active_idx;
alter index nodes_name_active_idx           rename to entities_name_active_idx;
alter index nodes_node_type_active_idx      rename to entities_entity_type_active_idx;

alter table relations rename column from_node_id to from_entity_id;
alter table relations rename column to_node_id   to to_entity_id;

alter table observations rename column node_id to entity_id;

alter table nodes rename to entities;
alter table entities rename column node_type to entity_type;
