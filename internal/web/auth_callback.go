package web

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
	oidcpkg "github.com/mariogutierrez/context-harness-mcp/internal/auth/oidc"
)

// exchangeFunc is the signature of the code-exchange function injected by tests.
// In production the field is nil and the real provider is used.
type exchangeFunc func(ctx context.Context, code, pkceVerifier string) (*oidcpkg.Identity, error)

// oidcCallbackHandler handles GET /auth/callback in OIDC mode.
// This handler is the security-critical termination point of the Authorization
// Code + PKCE flow. Each step maps to a specific threat in the security checklist.
type oidcCallbackHandler struct {
	pool    *pgxpool.Pool
	baseURL string
	// exchangeOverride is non-nil only in tests. When set it replaces the real
	// provider code exchange, allowing the nonce-mismatch branch (step 6) to
	// be exercised without a live OIDC provider or Docker (AC-3 test seam).
	exchangeOverride exchangeFunc
}

// ServeHTTP implements http.Handler for GET /auth/callback (OIDC mode).
//
// Flow (each step is a security gate):
//  1. Reject non-GET methods.
//  2. Read ephemeral state cookie — absent/tampered = 4xx (threat #10).
//  3. Compare state param vs cookie.state (constant-time) — mismatch = 4xx (threat #2).
//  4. Exchange authorization code + PKCE verifier (threat #1).
//  5. go-oidc verifies id_token: signature, iss, aud, exp, alg pin (threats #4, #5, #6).
//  6. Compare id_token.Nonce vs cookie.nonce (constant-time) — mismatch = 4xx (threat #3).
//  7. Upsert user + issue MCP JWT + issue ch_session (atomic tx via mintTokenTx).
//  8. Clear ephemeral state cookie (single-use semantics, threat #10).
//  9. 302 to validated next (IsSafe) or /dashboard.
func (h *oidcCallbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Step 2: read ephemeral state cookie.
	cookieState, cookieNonce, pkceVerifier, next, err := oidcpkg.ReadStateCookie(r)
	if err != nil {
		slog.Warn("web: oidc callback: invalid state cookie", "error", err)
		auth.WriteError(w, auth.CodeInvalidSupabaseToken, h.baseURL)
		return
	}

	// Step 3: CSRF protection — compare state parameter vs cookie (constant-time).
	// Threat #2 (CWE-352). Mismatch means a forged or replayed callback.
	paramState := r.URL.Query().Get("state")
	if subtle.ConstantTimeCompare([]byte(paramState), []byte(cookieState)) != 1 {
		slog.Warn("web: oidc callback: state mismatch (possible CSRF)")
		// Clear the cookie to prevent reuse of the tampered value.
		oidcpkg.ClearStateCookie(w, r)
		auth.WriteError(w, auth.CodeInvalidSupabaseToken, h.baseURL)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		slog.Warn("web: oidc callback: missing code parameter")
		oidcpkg.ClearStateCookie(w, r)
		auth.WriteError(w, auth.CodeExchangeMalformed, h.baseURL)
		return
	}

	// Check for error from provider (e.g. user denied consent).
	// Log only the error code (a bounded OAuth2 enum), not the free-text
	// error_description which is attacker-influenced and may contain newlines
	// or control characters that enable log injection (fix: SEC-004, CWE-117).
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		slog.Warn("web: oidc callback: provider returned error",
			"error", sanitizeLogField(errParam),
		)
		oidcpkg.ClearStateCookie(w, r)
		auth.WriteError(w, auth.CodeInvalidSupabaseToken, h.baseURL)
		return
	}

	// Step 4+5: exchange code + verify id_token (sig, iss, aud, exp, alg).
	// exchangeOverride is non-nil only in tests; production always uses the
	// real provider (see exchangeFunc type definition above).
	exchange := h.exchangeOverride
	if exchange == nil {
		provider, provErr := oidcpkg.NewProvider(context.Background())
		if provErr != nil {
			slog.Error("web: oidc callback: provider init failed", "error", provErr)
			oidcpkg.ClearStateCookie(w, r)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		exchange = provider.Exchange
	}

	identity, err := exchange(r.Context(), code, pkceVerifier)
	if err != nil {
		slog.Warn("web: oidc callback: exchange/verify failed", "error", err)
		oidcpkg.ClearStateCookie(w, r)
		auth.WriteError(w, auth.CodeInvalidSupabaseToken, h.baseURL)
		return
	}

	// Step 6: nonce validation — threat #3 (CWE-294).
	// go-oidc does NOT validate nonce; the server does it here explicitly.
	// Constant-time compare prevents timing oracle on the nonce value.
	if subtle.ConstantTimeCompare([]byte(identity.Nonce), []byte(cookieNonce)) != 1 {
		slog.Warn("web: oidc callback: nonce mismatch (possible replay attack)",
			"sub_prefix", subPrefix(identity.Sub),
		)
		oidcpkg.ClearStateCookie(w, r)
		auth.WriteError(w, auth.CodeInvalidSupabaseToken, h.baseURL)
		return
	}

	// Step 7: atomic transaction — upsert user, issue JWT, issue session cookie.
	cfg := oidcpkg.LoadOIDCConfig()
	issuer := cfg.IssuerURL

	_, err = mintTokenTx(r.Context(), w, r, h.pool, issuer, identity.Sub, identity.Email, h.baseURL)
	if err != nil {
		// mintTokenTx already wrote an error response.
		oidcpkg.ClearStateCookie(w, r)
		return
	}

	// Step 8: clear ephemeral state cookie (single-use).
	oidcpkg.ClearStateCookie(w, r)

	// Step 9: redirect to validated destination.
	dest := "/dashboard"
	if IsSafe(next) {
		dest = next
	}
	http.Redirect(w, r, dest, http.StatusFound)
}

// RegisterOIDCCallback registers GET /auth/callback with the OIDC handler on mux.
// The pool is used for the atomic upsert+JWT+session transaction.
func RegisterOIDCCallback(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := &oidcCallbackHandler{
		pool:    pool,
		baseURL: os.Getenv("MCP_PUBLIC_URL"),
	}
	mux.Handle("/auth/callback", h)
}

// sanitizeLogField strips CR, LF, and other ASCII control characters from a
// string before it is written to a structured log field. Prevents log injection
// via attacker-controlled values such as OAuth2 error_description (CWE-117).
func sanitizeLogField(s string) string {
	result := make([]byte, 0, len(s))
	for i := range len(s) {
		c := s[i]
		// Keep printable ASCII and non-ASCII bytes; strip control chars (0x00-0x1F, 0x7F).
		if c >= 0x20 && c != 0x7F {
			result = append(result, c)
		}
	}
	return string(result)
}
