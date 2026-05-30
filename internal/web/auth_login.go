package web

import (
	"context"
	"log/slog"
	"net/http"

	oidcpkg "github.com/mariogutierrez/context-harness-mcp/internal/auth/oidc"
)

// oidcLoginHandler handles GET /auth/login in OIDC mode.
// It generates a fresh state/nonce/PKCE bundle, stores it in an ephemeral
// signed cookie, and issues a 302 redirect to the provider's authorization
// endpoint.
type oidcLoginHandler struct{}

// ServeHTTP implements http.Handler for GET /auth/login (OIDC mode).
//
// Flow:
//  1. If the user already has a valid session, redirect to /dashboard.
//  2. Generate state, nonce, PKCE verifier (32 random bytes each).
//  3. Store them in a signed, HttpOnly, SameSite=Lax cookie (Max-Age=600).
//  4. Discover the OIDC provider (lazy, cached via sync.Once).
//  5. Issue 302 to provider authorize URL with PKCE+state+nonce.
func (h *oidcLoginHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	// Already authenticated — skip the login flow.
	if _, err := Read(r); err == nil {
		dest := "/dashboard"
		if n := safeNext(r); n != "" {
			dest = n
		}
		http.Redirect(w, r, dest, http.StatusFound)
		return
	}

	next := safeNext(r)

	state, nonce, pkceVerifier, err := oidcpkg.GenerateState()
	if err != nil {
		slog.Error("web: oidc login: generate state failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := oidcpkg.IssueStateCookie(w, r, state, nonce, pkceVerifier, next); err != nil {
		slog.Error("web: oidc login: issue state cookie failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	provider, err := oidcpkg.NewProvider(context.Background())
	if err != nil {
		slog.Error("web: oidc login: provider init failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	authURL := provider.AuthCodeURL(state, nonce, pkceVerifier)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// RegisterOIDCLogin registers GET /auth/login with the OIDC handler on mux.
// Call this only when OIDC mode is configured; in legacy mode use RegisterLogin.
func RegisterOIDCLogin(mux *http.ServeMux) {
	mux.Handle("/auth/login", &oidcLoginHandler{})
}
