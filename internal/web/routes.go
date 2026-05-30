package web

import (
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
)

// RegisterWebAuthRoutes registers all unauthenticated auth-related HTTP routes
// on the mux, selecting between OIDC mode and legacy Supabase mode based on
// environment configuration.
//
// Mode selection:
//   - OIDC_ISSUER_URL present → OIDC mode: /auth/login (OIDC redirect) +
//     /auth/callback (server-side code exchange + verification).
//     POST /auth/exchange is NOT registered in OIDC mode (Supabase implicit flow
//     is replaced by server-side authorization code flow).
//   - OIDC_ISSUER_URL absent → Supabase legacy mode: the original /auth/login
//     (render login.html), /auth/callback (render callback.html for JS implicit
//     flow), and POST /auth/exchange (token validation + JWT issuance).
func RegisterWebAuthRoutes(mux *http.ServeMux, pool *pgxpool.Pool, authMode auth.Mode) {
	if os.Getenv("OIDC_ISSUER_URL") != "" {
		// OIDC mode: server-side Authorization Code + PKCE flow.
		// /auth/login → generate state/nonce/PKCE, redirect to provider.
		// /auth/callback → validate state, exchange code, verify id_token, mint JWT.
		RegisterOIDCLogin(mux)
		RegisterOIDCCallback(mux, pool)
		// Legacy exchange route is intentionally omitted in OIDC mode.
		// Clients using the old POST /auth/exchange endpoint will receive 404,
		// which surfaces the migration requirement clearly.
		return
	}

	// Supabase legacy mode — all three original routes.
	RegisterCallback(mux)
	RegisterExchange(mux, pool)
	RegisterLogin(mux)
}
