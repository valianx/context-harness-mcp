-- +goose Up
-- v1.1.0 — OIDC provider-agnostic auth.
-- Adds provider identity columns. Existing Supabase rows backfill to
-- auth_provider='supabase', external_subject = supabase_user_id (as text).
-- NOT NULL DEFAULT on auth_provider is metadata-only in PG11+ (fast path,
-- no table rewrite). external_subject is backfilled from the existing PK.
alter table users
  add column auth_provider   text not null default 'supabase';

alter table users
  add column external_subject text;

-- Backfill external_subject from the existing Supabase UUID PK for all
-- pre-OIDC rows. New OIDC rows will set it explicitly at upsert time.
update users
  set external_subject = supabase_user_id::text
  where external_subject is null;

-- Uniqueness of an identity is (provider, subject). A user from Google and a
-- user from Supabase could in theory collide on raw subject; the provider
-- qualifier prevents that.
create unique index users_provider_subject_key
  on users (auth_provider, external_subject)
  where external_subject is not null;

-- Lookup index for the CLI revoke-by-subject path.
create index users_external_subject_idx
  on users (external_subject)
  where external_subject is not null;

-- +goose Down
-- Dev/CI only — NEVER run in prod (forward-only per docs/knowledge.md [patrón]).
drop index if exists users_external_subject_idx;
drop index if exists users_provider_subject_key;
alter table users drop column if exists external_subject;
alter table users drop column if exists auth_provider;
