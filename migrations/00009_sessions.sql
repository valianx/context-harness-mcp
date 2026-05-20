-- +goose Up
-- Phase 4 — sessions data model.
-- A session is a bracketed unit of work: session_start → ... → session_end.
-- Sessions are tags, not security boundaries — same trust model as project_id.
-- Cross-project sessions are allowed; project_id here is denormalized for
-- filter speed, captured at session_start time.

-- +goose StatementBegin

CREATE TABLE sessions (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      uuid REFERENCES users(supabase_user_id) ON DELETE SET NULL,
  project_id   text NOT NULL DEFAULT 'global',
  working_dir  text,
  started_at   timestamptz NOT NULL DEFAULT now(),
  ended_at     timestamptz,
  summary      text
);

-- Filter helpers — session listing by user / project.
CREATE INDEX sessions_user_id_idx    ON sessions(user_id)    WHERE user_id IS NOT NULL;
CREATE INDEX sessions_project_id_idx ON sessions(project_id);

-- Attach the session to each node it created. ON DELETE SET NULL means
-- deleting a session (rare; sessions are not soft-deleted by tool surface)
-- detaches the link without breaking the node.
ALTER TABLE nodes ADD COLUMN session_id uuid REFERENCES sessions(id) ON DELETE SET NULL;

-- Partial index for the session_summary query: fetch all active+deleted nodes
-- attached to a given session, ordered by created_at.
CREATE INDEX nodes_session_id_idx ON nodes(session_id, created_at) WHERE session_id IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- Dev/CI only — NEVER run in prod (per docs/knowledge.md [patrón] forward-only).
-- +goose StatementBegin
ALTER TABLE nodes DROP COLUMN IF EXISTS session_id;
DROP TABLE IF EXISTS sessions;
-- +goose StatementEnd
