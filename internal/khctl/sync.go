package khctl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

// supabaseAdminUser is the subset of the Supabase Admin API user object that
// the sync reconciliation logic needs. Unknown fields are ignored.
type supabaseAdminUser struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	BannedUntil *string `json:"banned_until"`
	DeletedAt   *string `json:"deleted_at"`
}

// supabaseAdminListResponse matches the Admin API paginated response shape for
// GET /auth/v1/admin/users.
type supabaseAdminListResponse struct {
	Users []supabaseAdminUser `json:"users"`
	// The Admin API does not return a next_page token — we iterate until an
	// empty page is returned.
}

// SyncUsers reconciles public.users against the Supabase Admin API user list.
//
// It connects to the local DB via dsn, paginates through all Supabase users
// (100 per page), and applies three reconciliation rules:
//  1. Local user NOT in Supabase list → revoke (set revoked_at = now()).
//  2. Local user with revoked_at IS NULL but Supabase banned_until is a future
//     timestamp or deleted_at is non-null → revoke.
//  3. Local user with revoked_at IS NOT NULL but Supabase user is active
//     (banned_until IS NULL, deleted_at IS NULL) → un-revoke (set revoked_at = NULL).
//
// Every change is logged as a structured slog.Info line.
// Errors during reconciliation are logged but do not abort the run — the cron
// workflow is resilient by design (per [restricción] cron resilience).
func SyncUsers(ctx context.Context, dsn, supabaseServiceRoleKey, supabaseProjectURL string) error {
	pool, err := connectPool(ctx, dsn)
	if err != nil {
		return fmt.Errorf("sync: connect to DB: %w", err)
	}
	defer pool.Close()

	supabaseUsers, err := fetchAllSupabaseUsers(ctx, supabaseProjectURL, supabaseServiceRoleKey)
	if err != nil {
		return fmt.Errorf("sync: fetch Supabase users: %w", err)
	}

	localUsers, err := fetchAllLocalUsers(ctx, pool)
	if err != nil {
		return fmt.Errorf("sync: fetch local users: %w", err)
	}

	reconcile(ctx, pool, localUsers, supabaseUsers)
	return nil
}

// connectPool opens a pgxpool connection using the given DSN.
func connectPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// fetchAllSupabaseUsers pages through the Supabase Admin API until an empty
// page is returned, collecting all user records.
func fetchAllSupabaseUsers(ctx context.Context, projectURL, serviceRoleKey string) (map[string]supabaseAdminUser, error) {
	users := make(map[string]supabaseAdminUser)
	client := &http.Client{Timeout: 30 * time.Second}

	for page := 1; ; page++ {
		pageUsers, err := fetchSupabasePage(ctx, client, projectURL, serviceRoleKey, page)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		if len(pageUsers) == 0 {
			break
		}
		for _, u := range pageUsers {
			users[u.ID] = u
		}
		slog.Info("sync: fetched Supabase page", "page", page, "count", len(pageUsers))
	}

	slog.Info("sync: total Supabase users fetched", "total", len(users))
	return users, nil
}

// fetchSupabasePage calls GET /auth/v1/admin/users?per_page=100&page=N and
// returns the user list for that page.
func fetchSupabasePage(ctx context.Context, client *http.Client, projectURL, serviceRoleKey string, page int) ([]supabaseAdminUser, error) {
	url := fmt.Sprintf("%s/auth/v1/admin/users?per_page=100&page=%d", projectURL, page)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+serviceRoleKey)
	req.Header.Set("apikey", serviceRoleKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result supabaseAdminListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return result.Users, nil
}

// fetchAllLocalUsers returns all rows from public.users as a map keyed by
// supabase_user_id.
func fetchAllLocalUsers(ctx context.Context, pool *pgxpool.Pool) (map[string]*store.UserRow, error) {
	rows, err := pool.Query(ctx,
		`SELECT supabase_user_id, email, revoked_at, created_at, updated_at FROM users`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make(map[string]*store.UserRow)
	for rows.Next() {
		var u store.UserRow
		if err := rows.Scan(&u.SupabaseUserID, &u.Email, &u.RevokedAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		users[u.SupabaseUserID] = &u
	}
	return users, rows.Err()
}

// reconcile applies the three sync rules to each local user.
// Errors from individual SetUserRevoked calls are logged but do not abort
// processing of other users (cron resilience).
func reconcile(ctx context.Context, pool *pgxpool.Pool, localUsers map[string]*store.UserRow, supabaseUsers map[string]supabaseAdminUser) {
	for id, local := range localUsers {
		su, existsInSupabase := supabaseUsers[id]
		if !existsInSupabase {
			revokeUser(ctx, pool, id, local.Email, "not found in Supabase")
			continue
		}
		applyBanReconciliation(ctx, pool, local, su)
	}
}

// applyBanReconciliation compares the local revocation state against the
// Supabase ban state and applies the appropriate update.
func applyBanReconciliation(ctx context.Context, pool *pgxpool.Pool, local *store.UserRow, su supabaseAdminUser) {
	supabaseRevoked := isFutureTimestamp(su.BannedUntil) || su.DeletedAt != nil
	localRevoked := local.RevokedAt != nil

	switch {
	case !localRevoked && supabaseRevoked:
		revokeUser(ctx, pool, local.SupabaseUserID, local.Email, "Supabase banned_until/deleted_at set")
	case localRevoked && !supabaseRevoked:
		unrevokeUser(ctx, pool, local.SupabaseUserID, local.Email)
	default:
		// States match — no action needed.
	}
}

// revokeUser sets revoked_at = now() for the given user.
func revokeUser(ctx context.Context, pool *pgxpool.Pool, id, email, reason string) {
	now := time.Now()
	if err := store.SetUserRevoked(ctx, pool, id, &now); err != nil {
		slog.Error("sync: failed to revoke user", "sub_prefix", subPrefix(id), "reason", reason, "error", err)
		return
	}
	slog.Info("sync: user revoked", "sub_prefix", subPrefix(id), "email", email, "reason", reason)
}

// unrevokeUser sets revoked_at = NULL for the given user.
func unrevokeUser(ctx context.Context, pool *pgxpool.Pool, id, email string) {
	if err := store.SetUserRevoked(ctx, pool, id, nil); err != nil {
		slog.Error("sync: failed to un-revoke user", "sub_prefix", subPrefix(id), "error", err)
		return
	}
	slog.Info("sync: user un-revoked", "sub_prefix", subPrefix(id), "email", email)
}

// isFutureTimestamp returns true when s is a non-null RFC3339 timestamp that
// is strictly after the current time. Mirrors the webhook handler's logic.
func isFutureTimestamp(s *string) bool {
	if s == nil {
		return false
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05.999999Z07:00", *s)
		if err != nil {
			return false
		}
	}
	return t.After(time.Now())
}

// subPrefix is duplicated here to keep the khctl package independent of web.
// Returns the first 8 characters of a UUID for safe log correlation.
func subPrefix(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}
