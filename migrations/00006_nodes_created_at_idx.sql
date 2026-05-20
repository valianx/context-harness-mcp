-- +goose Up
-- Partial index for timeline queries: ORDER BY created_at DESC, id DESC with
-- optional since/until bounds. The partial predicate (deleted_at IS NULL) keeps
-- the index small and co-located with the WHERE clause used by ListByCreatedAt.
create index nodes_created_at_idx on nodes (created_at DESC, id DESC)
  where deleted_at is null;

-- +goose Down
drop index if exists nodes_created_at_idx;
