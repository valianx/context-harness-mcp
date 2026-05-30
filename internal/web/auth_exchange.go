package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
)

// defaultSnippetServerName is the MCP server name used in the ~/.claude.json
// snippet when MCP_SNIPPET_SERVER_NAME is not set.
const defaultSnippetServerName = "context-harness"

// exchangeRequest is the expected POST /auth/exchange body.
// The optional Next field supports the ?next= redirect flow: the callback page
// reads next from the URL and passes it here; the server validates it via
// IsSafe and echoes a safe redirect_to in the response.
type exchangeRequest struct {
	AccessToken string `json:"access_token"`
	Next        string `json:"next,omitempty"`
}

// exchangeResponse is the 200 OK body returned by POST /auth/exchange.
// RedirectTo is the server-validated destination URL the callback page should
// navigate to after a successful exchange. Defaults to "/dashboard".
// Legacy clients that ignore this field continue to work unchanged (AC-10).
type exchangeResponse struct {
	Token      string `json:"token"`
	ExpiresAt  string `json:"expires_at"`
	Snippet    string `json:"snippet"`
	RedirectTo string `json:"redirect_to"`
}

// tokenIssuer is the function type for issuing an MCP JWT.
// Expressed as a type so tests can inject a failing stub (AC-5).
type tokenIssuer func(sub, email string) (string, error)

// exchangeHandler handles POST /auth/exchange.
// It is constructed with its dependencies (SupabaseClient, DB pool, token
// issuer) to allow unit testing with mocks and stubs.
type exchangeHandler struct {
	supabase    auth.SupabaseClient
	pool        *pgxpool.Pool
	issueToken  tokenIssuer
	baseURL     string
	serverName  string
}

// newExchangeHandler builds a production exchange handler using env-sourced
// values and real Supabase + DB dependencies.
func newExchangeHandler(pool *pgxpool.Pool) *exchangeHandler {
	projectURL := os.Getenv("SUPABASE_PROJECT_URL")
	anonKey := os.Getenv("SUPABASE_ANON_KEY")

	serverName := os.Getenv("MCP_SNIPPET_SERVER_NAME")
	if serverName == "" {
		serverName = defaultSnippetServerName
	}

	return &exchangeHandler{
		supabase:   auth.NewSupabaseClient(projectURL, anonKey),
		pool:       pool,
		issueToken: auth.IssueMCPToken,
		baseURL:    os.Getenv("MCP_PUBLIC_URL"),
		serverName: serverName,
	}
}

// ServeHTTP handles POST /auth/exchange.
// Flow (atomic):
//  1. Parse and validate request body.
//  2. Call Supabase GET /auth/v1/user to validate the access token.
//  3. Check email_confirmed_at.
//  4. BEGIN transaction.
//  5. UPSERT public.users (clears revoked_at on re-login).
//  6. Issue MCP JWT — on error: ROLLBACK + 500.
//  7. COMMIT.
//  8. Return 200 with token, expires_at, snippet.
func (h *exchangeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	req, ok := h.parseRequest(w, r)
	if !ok {
		return
	}

	user, ok := h.validateSupabaseToken(w, r.Context(), req.AccessToken)
	if !ok {
		return
	}

	if user.EmailConfirmedAt == nil {
		auth.WriteError(w, auth.CodeEmailNotConfirmed, h.baseURL)
		return
	}

	token, expiresAt, ok := h.exchangeWithinTx(w, r, user)
	if !ok {
		return
	}

	redirectTo := "/dashboard"
	if IsSafe(req.Next) {
		redirectTo = req.Next
	}

	snippet := buildSnippet(h.serverName, h.baseURL, r.Host, token)

	writeJSON(w, exchangeResponse{
		Token:      token,
		ExpiresAt:  expiresAt,
		Snippet:    snippet,
		RedirectTo: redirectTo,
	})
}

// parseRequest decodes the POST body. Writes 400 and returns false on error.
func (h *exchangeHandler) parseRequest(w http.ResponseWriter, r *http.Request) (exchangeRequest, bool) {
	var req exchangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.AccessToken == "" {
		auth.WriteError(w, auth.CodeExchangeMalformed, h.baseURL)
		return exchangeRequest{}, false
	}
	return req, true
}

// validateSupabaseToken calls Supabase to verify the access token.
// Writes 401 and returns false when Supabase rejects the token.
func (h *exchangeHandler) validateSupabaseToken(w http.ResponseWriter, ctx context.Context, accessToken string) (*auth.SupabaseUser, bool) {
	user, err := h.supabase.GetUser(ctx, accessToken)
	if err != nil {
		if errors.Is(err, auth.ErrSupabaseUnauthorized) {
			auth.WriteError(w, auth.CodeInvalidSupabaseToken, h.baseURL)
		} else {
			slog.Error("web: supabase GetUser error", "error", err)
			auth.WriteError(w, auth.CodeJWTIssuanceFailed, h.baseURL)
		}
		return nil, false
	}
	return user, true
}

// exchangeWithinTx runs the DB transaction: upsert user, issue JWT, issue
// session cookie, commit. Delegates to mintTokenTx using the Supabase legacy
// provider path. Returns the signed token and RFC3339 expires_at, or writes
// an error response and returns false.
func (h *exchangeHandler) exchangeWithinTx(w http.ResponseWriter, r *http.Request, user *auth.SupabaseUser) (string, string, bool) {
	// Use "supabase" as the provider for legacy Supabase exchange flows.
	// The subject is the Supabase user UUID (already a valid UUID, so
	// DeriveUserID will use it as-is as supabase_user_id).
	result, err := mintTokenTx(r.Context(), w, r, h.pool, "supabase", user.ID, user.Email, h.baseURL)
	if err != nil {
		return "", "", false
	}
	return result.Token, result.ExpiresAt, true
}

// buildSnippet returns a pretty-printed JSON string ready to paste into
// ~/.claude.json under the mcpServers key.
//
// Schema: flat (type, url, headers as siblings — NOT wrapped in a "transport"
// object). Claude Code's MCP HTTP transport reads this schema; the wrapped
// variant is silently rejected and the server reports HTTP 404. URL ends in
// "/mcp/" (trailing slash) because Claude Code's HTTP client calls "/mcp/" and
// Go's ServeMux distinguishes /mcp from /mcp/.
func buildSnippet(serverName, mcpPublicURL, host, token string) string {
	base := mcpPublicURL
	if base == "" {
		base = "http://" + host
	}

	type serverEntry struct {
		Type    string            `json:"type"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
	}
	type snippet struct {
		MCPServers map[string]serverEntry `json:"mcpServers"`
	}

	s := snippet{
		MCPServers: map[string]serverEntry{
			serverName: {
				Type: "http",
				URL:  base + "/mcp/",
				Headers: map[string]string{
					"Authorization": "Bearer " + token,
				},
			},
		},
	}

	data, _ := json.MarshalIndent(s, "", "  ")
	return string(data)
}

// jwtExpiry returns the configured JWT lifetime (mirrors internal/auth/jwt.go).
// We need the duration here to compute expires_at for the response body without
// re-exposing it from the auth package.
func jwtExpiry() time.Duration {
	raw := os.Getenv("MCP_JWT_EXPIRY")
	if raw == "" {
		return 8760 * time.Hour // 1 year default
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 8760 * time.Hour
	}
	return d
}

// writeJSON serializes v as JSON and writes it with status 200.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("web: json encode failed", "error", err)
	}
}

// subPrefix returns the first 8 characters of a UUID for safe log output.
// Logging the full sub is acceptable (it is not a secret), but the prefix is
// sufficient for correlation and avoids log-search false positives.
func subPrefix(sub string) string {
	if len(sub) >= 8 {
		return sub[:8]
	}
	return sub
}

// isErrNoTx returns true for the pgx "transaction already committed or rolled back"
// sentinel so we can suppress the spurious rollback error in the defer.
func isErrNoTx(err error) bool {
	// pgx v5 returns this error string when Commit was already called.
	return err != nil && err.Error() == "tx is closed"
}

// RegisterExchange registers POST /auth/exchange on the given mux.
// pool must be non-nil. The route is unauthenticated by design — auth.Middleware
// is NOT applied here; the Supabase access token in the request body is the
// only credential accepted.
func RegisterExchange(mux *http.ServeMux, pool *pgxpool.Pool) {
	h := newExchangeHandler(pool)
	mux.Handle("/auth/exchange", h)
}
