-- +goose Up
-- Phase 0 — table tracking users authorized to use this MCP instance.
-- supabase_user_id mirrors auth.users.id from the Supabase Auth schema.
-- revoked_at NULL = active; non-null = revoked (webhook or cron sets it).
create table users (
  supabase_user_id  uuid        primary key,
  email             text        not null unique,
  revoked_at        timestamptz,
  created_at        timestamptz not null default now(),
  updated_at        timestamptz not null default now()
);

-- Partial index: fast lookup of active users (the hot path for middleware).
create index users_active_idx on users (supabase_user_id)
  where revoked_at is null;

-- Email lookup for the cron sync and audit queries.
create index users_email_idx on users (email);

-- +goose Down
drop table if exists users;
