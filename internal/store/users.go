// Package store provides Postgres access for the context-harness-mcp server.
// This file contains the users table helpers for the Phase 0 auth feature.
package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UserRow represents a row from the public.users table.
type UserRow struct {
	SupabaseUserID string
	Email          string
	RevokedAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UpsertUser inserts or updates a user row inside an existing transaction.
// On conflict (supabase_user_id already exists), it updates the email and
// clears revoked_at to NULL — re-login un-revokes per the locked decision.
// All SQL is parameterized per [restricción] in docs/knowledge.md.
func UpsertUser(ctx context.Context, tx pgx.Tx, supabaseUserID, email string) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO users (supabase_user_id, email)
		 VALUES ($1, $2)
		 ON CONFLICT (supabase_user_id) DO UPDATE
		   SET email      = EXCLUDED.email,
		       updated_at = now(),
		       revoked_at = NULL`,
		supabaseUserID,
		email,
	)
	return err
}

// GetUserByID returns the user row for the given supabaseUserID using a pool
// connection (non-transactional read path). Returns pgx.ErrNoRows when the
// user is not found.
func GetUserByID(ctx context.Context, pool *pgxpool.Pool, supabaseUserID string) (*UserRow, error) {
	row := pool.QueryRow(ctx,
		`SELECT supabase_user_id, email, revoked_at, created_at, updated_at
		 FROM users
		 WHERE supabase_user_id = $1`,
		supabaseUserID,
	)

	var u UserRow
	if err := row.Scan(&u.SupabaseUserID, &u.Email, &u.RevokedAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

// SetUserRevoked updates the revoked_at timestamp for the given user using the
// pool directly (used by the webhook handler, which operates outside a Tx).
// Pass nil revokedAt to un-revoke (set revoked_at = NULL).
func SetUserRevoked(ctx context.Context, pool *pgxpool.Pool, supabaseUserID string, revokedAt *time.Time) error {
	_, err := pool.Exec(ctx,
		`UPDATE users
		 SET revoked_at = $1,
		     updated_at = now()
		 WHERE supabase_user_id = $2`,
		revokedAt,
		supabaseUserID,
	)
	return err
}

// DeleteUser removes a user row inside an existing transaction.
// Used as a compensating operation in /auth/exchange when JWT issuance fails
// after a new user was inserted — ensures no orphan row is left in the table.
// For existing users (upsert updated the row), this should NOT be called;
// the handler must track whether the row is new before calling this.
func DeleteUser(ctx context.Context, tx pgx.Tx, supabaseUserID string) error {
	_, err := tx.Exec(ctx,
		`DELETE FROM users WHERE supabase_user_id = $1`,
		supabaseUserID,
	)
	return err
}
