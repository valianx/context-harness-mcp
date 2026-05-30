package web

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
	"github.com/mariogutierrez/context-harness-mcp/internal/store"
)

// mintResult holds the outputs of a successful mintTokenTx call.
type mintResult struct {
	Token     string
	ExpiresAt string
}

// mintTokenTx performs the atomic upsert + JWT issuance + session-cookie write
// inside a new pgx transaction. It is the shared implementation used by both
// the OIDC callback handler and the legacy Supabase exchange handler.
//
// Transaction semantics:
//   - BEGIN, then UpsertUserByProvider (or UpsertUser for Supabase legacy).
//   - IssueMCPToken — on failure: ROLLBACK so no orphan row is left.
//   - Issue ch_session cookie — on failure: ROLLBACK.
//   - COMMIT — on failure: log and return error.
//
// The session cookie is written to w before Commit so Set-Cookie appears in
// the same HTTP response as the JSON body (or the redirect response).
// If Commit fails the cookie is already sent; the caller should not redirect
// in that case (the handler returns a 500 instead).
func mintTokenTx(
	ctx context.Context,
	w http.ResponseWriter,
	r *http.Request,
	pool *pgxpool.Pool,
	provider, subject, email string,
	baseURL string,
) (*mintResult, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		slog.Error("web: mint: begin transaction failed", "error", err)
		auth.WriteError(w, auth.CodeJWTIssuanceFailed, baseURL)
		return nil, err
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !isErrNoTx(rbErr) {
			slog.Error("web: mint: rollback failed", "error", rbErr)
		}
	}()

	userID, err := store.UpsertUserByProvider(ctx, tx, provider, subject, email)
	if err != nil {
		slog.Error("web: mint: upsert user failed", "error", err, "sub_prefix", subPrefix(subject))
		auth.WriteError(w, auth.CodeJWTIssuanceFailed, baseURL)
		return nil, err
	}

	token, err := auth.IssueMCPToken(userID, email)
	if err != nil {
		slog.Error("web: mint: IssueMCPToken failed", "error", err, "sub_prefix", subPrefix(userID))
		auth.WriteError(w, auth.CodeJWTIssuanceFailed, baseURL)
		return nil, err
	}

	if err := Issue(w, r, userID); err != nil {
		slog.Error("web: mint: session Issue failed", "error", err, "sub_prefix", subPrefix(userID))
		auth.WriteError(w, auth.CodeJWTIssuanceFailed, baseURL)
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		slog.Error("web: mint: commit failed", "error", err, "sub_prefix", subPrefix(userID))
		auth.WriteError(w, auth.CodeJWTIssuanceFailed, baseURL)
		return nil, err
	}

	expiresAt := time.Now().Add(jwtExpiry()).Format(time.RFC3339)
	return &mintResult{Token: token, ExpiresAt: expiresAt}, nil
}
