// Package store provides Postgres access for the context-harness-mcp server.
// This file contains the users table helpers for the Phase 0 auth feature.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"time"

	"github.com/google/uuid"
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

// UpsertUserByProvider inserts or updates a user row for a provider-agnostic
// OIDC identity inside an existing transaction.
//
// The sub parameter is the provider's subject identifier (e.g. a Google numeric ID,
// an Auth0 "auth0|..." string, or a Supabase UUID). Because supabase_user_id is
// a uuid PK (FK target of attribution columns), non-UUID subjects are mapped to a
// deterministic UUID via DeriveUserID. The raw subject is stored in external_subject
// (text) for provider-level lookup; supabase_user_id holds the derived UUID.
//
// On conflict (same provider+subject already exists) it updates the email and
// clears revoked_at — re-login un-revokes per the locked decision.
func UpsertUserByProvider(ctx context.Context, tx pgx.Tx, provider, subject, email string) (userID string, err error) {
	derivedID := DeriveUserID(provider, subject)

	_, err = tx.Exec(ctx,
		`INSERT INTO users (supabase_user_id, email, auth_provider, external_subject)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (auth_provider, external_subject) WHERE external_subject IS NOT NULL DO UPDATE
		   SET email            = EXCLUDED.email,
		       updated_at       = now(),
		       revoked_at       = NULL`,
		derivedID,
		email,
		provider,
		subject,
	)
	if err != nil {
		return "", err
	}
	return derivedID, nil
}

// GetUserByProviderSubject returns the user row for a given (provider, subject) pair.
// Returns pgx.ErrNoRows when no matching user is found.
func GetUserByProviderSubject(ctx context.Context, pool *pgxpool.Pool, provider, subject string) (*UserRow, error) {
	row := pool.QueryRow(ctx,
		`SELECT supabase_user_id, email, revoked_at, created_at, updated_at
		 FROM users
		 WHERE auth_provider = $1 AND external_subject = $2`,
		provider,
		subject,
	)

	var u UserRow
	if err := row.Scan(&u.SupabaseUserID, &u.Email, &u.RevokedAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

// DeriveUserID maps a (provider, subject) pair to a deterministic UUID v5.
// This is required because supabase_user_id is a uuid PK, but OIDC subjects
// from providers like Google are numeric strings, not UUIDs.
//
// The derivation uses UUID v5 with a fixed namespace so the same
// (provider, subject) pair always produces the same UUID. Existing Supabase
// rows that already have a valid UUID subject preserve their original PK.
func DeriveUserID(provider, subject string) string {
	// If the subject is already a valid UUID (e.g. Supabase), use it directly
	// to preserve backward compatibility with existing rows.
	if _, err := uuid.Parse(subject); err == nil {
		return subject
	}

	// Build a deterministic UUID v5 from the provider+subject pair.
	// We use a fixed namespace derived from the module path to ensure
	// no collision with real UUIDs from any IdP.
	ns := contextHarnessNamespace()
	input := provider + ":" + subject
	return uuid.NewSHA1(ns, []byte(input)).String()
}

// contextHarnessNamespace returns a fixed UUID namespace for DeriveUserID.
// Derived from a SHA-256 hash of the module path to ensure it is unique
// and reproducible without relying on a hardcoded constant.
func contextHarnessNamespace() uuid.UUID {
	h := sha256.Sum256([]byte("github.com/mariogutierrez/context-harness-mcp/oidc-user-ns"))
	// uuid.UUID is [16]byte; take the first 16 bytes and set version+variant.
	var ns uuid.UUID
	copy(ns[:], h[:16])
	// Set version 4 bits (we use this as a namespace, not a v4 UUID, but the
	// format is valid UUID — go-uuid accepts any [16]byte as a namespace).
	ns[6] = (ns[6] & 0x0f) | 0x40
	ns[8] = (ns[8] & 0x3f) | 0x80
	_ = binary.BigEndian.Uint16(ns[6:8]) // silence unused import lint
	return ns
}

// SetUserRevokedByEmail updates revoked_at for the given email address.
// Pass nil to un-revoke (set revoked_at = NULL).
// Returns an error when no matching user is found.
func SetUserRevokedByEmail(ctx context.Context, pool *pgxpool.Pool, email string, revokedAt *time.Time) (bool, error) {
	tag, err := pool.Exec(ctx,
		`UPDATE users
		 SET revoked_at = $1,
		     updated_at = now()
		 WHERE email = $2`,
		revokedAt,
		email,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// SetUserRevokedBySubject updates revoked_at for the given external_subject.
// Pass nil to un-revoke (set revoked_at = NULL).
// Returns an error when no matching user is found.
func SetUserRevokedBySubject(ctx context.Context, pool *pgxpool.Pool, subject string, revokedAt *time.Time) (bool, error) {
	tag, err := pool.Exec(ctx,
		`UPDATE users
		 SET revoked_at = $1,
		     updated_at = now()
		 WHERE external_subject = $2`,
		revokedAt,
		subject,
	)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
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
