-- +goose Up
-- Phase 0 — attribution columns for write tools.
-- Both nullable to preserve back-compat with rows created pre-auth and
-- with rows created via stdio transport (no user context).
-- FK uses ON DELETE SET NULL: if a user row is removed (admin cleanup),
-- the historical attribution is preserved as a free-form email + null UUID.
alter table nodes
  add column created_by_user_id uuid references users(supabase_user_id) on delete set null,
  add column created_by_email   text;

alter table observations
  add column created_by_user_id uuid references users(supabase_user_id) on delete set null,
  add column created_by_email   text;

alter table relations
  add column created_by_user_id uuid references users(supabase_user_id) on delete set null,
  add column created_by_email   text;

-- Index on user attribution for "what did <user> create" queries (future audit UI).
-- Partial: only indexed when set (most rows pre-auth will be NULL).
create index nodes_created_by_user_idx        on nodes        (created_by_user_id) where created_by_user_id is not null;
create index observations_created_by_user_idx on observations (created_by_user_id) where created_by_user_id is not null;
create index relations_created_by_user_idx    on relations    (created_by_user_id) where created_by_user_id is not null;

-- +goose Down
drop index if exists relations_created_by_user_idx;
drop index if exists observations_created_by_user_idx;
drop index if exists nodes_created_by_user_idx;
alter table relations    drop column if exists created_by_email, drop column if exists created_by_user_id;
alter table observations drop column if exists created_by_email, drop column if exists created_by_user_id;
alter table nodes        drop column if exists created_by_email, drop column if exists created_by_user_id;
