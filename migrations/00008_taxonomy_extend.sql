-- +goose Up
-- Phase 3 — extend relation taxonomy with supersedes and conflicts_with.
-- The CHECK constraint was inline-anonymous in 00001 (auto-named
-- `relations_relation_type_check`). DROP + ADD is non-blocking; all existing
-- rows satisfy the new (superset) CHECK so no validation failure can occur.

alter table relations drop constraint relations_relation_type_check;

alter table relations
  add constraint relations_relation_type_check
  check (relation_type in (
    'relates_to', 'belongs-to', 'calls', 'uses-stack', 'depends-on',
    'supersedes', 'conflicts_with'
  ));

-- +goose Down
-- Dev/CI only.
alter table relations drop constraint if exists relations_relation_type_check;
alter table relations
  add constraint relations_relation_type_check
  check (relation_type in (
    'relates_to', 'belongs-to', 'calls', 'uses-stack', 'depends-on'
  ));
