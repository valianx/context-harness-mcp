package khctl

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

// MatcherKind selects whether to match a user by email or external_subject.
type MatcherKind string

const (
	// MatchByEmail matches users by their email address.
	MatchByEmail MatcherKind = "email"
	// MatchBySubject matches users by their external_subject (provider subject).
	MatchBySubject MatcherKind = "subject"
)

// Revoke sets users.revoked_at = now() for the matching user.
// Returns (true, nil) when the user was found and updated.
// Returns (false, nil) when no user matched.
// Returns (false, err) on DB error.
func Revoke(ctx context.Context, pool *pgxpool.Pool, matcher MatcherKind, value string) (bool, error) {
	now := time.Now()
	switch matcher {
	case MatchBySubject:
		return store.SetUserRevokedBySubject(ctx, pool, value, &now)
	default: // MatchByEmail
		return store.SetUserRevokedByEmail(ctx, pool, value, &now)
	}
}

// Reinstate sets users.revoked_at = NULL for the matching user.
// Returns (true, nil) when the user was found and updated.
// Returns (false, nil) when no user matched.
// Returns (false, err) on DB error.
func Reinstate(ctx context.Context, pool *pgxpool.Pool, matcher MatcherKind, value string) (bool, error) {
	switch matcher {
	case MatchBySubject:
		return store.SetUserRevokedBySubject(ctx, pool, value, nil)
	default: // MatchByEmail
		return store.SetUserRevokedByEmail(ctx, pool, value, nil)
	}
}

// OpenPool opens a pgxpool connection to the given DSN. Callers are responsible
// for calling pool.Close() when done.
func OpenPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("revoke: open pool: %w", err)
	}
	return pool, nil
}
