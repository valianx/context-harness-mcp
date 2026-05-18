-- +goose Up
-- Enable the pgvector extension for vector(384) column support.
create extension if not exists vector;

create table entities (
  id          uuid        primary key default gen_random_uuid(),
  name        text        not null unique,
  entity_type text        not null check (entity_type in (
                'pattern', 'error', 'constraint', 'decision',
                'tool-gotcha', 'process-insight',
                'project', 'service', 'stack-profile'
              )),
  created_at  timestamptz not null default now(),
  updated_at  timestamptz not null default now(),
  deleted_at  timestamptz                          -- soft delete
);

-- Partial index: active-row entity_type lookups skip deleted rows at the index level.
create index entities_entity_type_active_idx on entities (entity_type)
  where deleted_at is null;

-- Partial index: active-row name lookups (used by open_nodes and dedup checks).
create index entities_name_active_idx on entities (name)
  where deleted_at is null;

create table observations (
  id        uuid        primary key default gen_random_uuid(),
  entity_id uuid        not null references entities(id) on delete cascade,
  text      text        not null,
  -- vector(384) matches all-MiniLM-L6-v2 output dims; null until PR-5 fills it.
  embedding vector(384),
  created_at timestamptz not null default now(),
  deleted_at timestamptz,
  unique (entity_id, text)                         -- DB-level dedup for import idempotency
);

-- Partial index: active-row joins from entities to their observations.
create index observations_entity_id_active_idx on observations (entity_id)
  where deleted_at is null;

-- HNSW cosine index for semantic search. m=16, ef_construction=64 are the
-- pgvector defaults and adequate for ≤100k vectors; tune if recall drops.
-- Partial: deleted observations are excluded from the ANN search entirely.
create index observations_embedding_hnsw on observations
  using hnsw (embedding vector_cosine_ops)
  with (m = 16, ef_construction = 64)
  where deleted_at is null;

create table relations (
  id             uuid        primary key default gen_random_uuid(),
  from_entity_id uuid        not null references entities(id) on delete cascade,
  to_entity_id   uuid        not null references entities(id) on delete cascade,
  relation_type  text        not null check (relation_type in (
                   'relates_to', 'belongs-to', 'calls', 'uses-stack', 'depends-on'
                 )),
  created_at     timestamptz not null default now(),
  deleted_at     timestamptz,
  unique (from_entity_id, to_entity_id, relation_type) -- no duplicate edges
);

-- Partial indexes: active-row traversal in both directions.
create index relations_from_active_idx on relations (from_entity_id)
  where deleted_at is null;
create index relations_to_active_idx   on relations (to_entity_id)
  where deleted_at is null;

-- +goose Down
drop table if exists relations;
drop table if exists observations;
drop table if exists entities;
drop extension if exists vector;
