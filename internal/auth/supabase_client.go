package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// SupabaseUser is the relevant subset of the Supabase GET /auth/v1/user response.
// Unknown fields are ignored (forward-compatibility with future Supabase schema changes).
type SupabaseUser struct {
	// ID is the Supabase auth.users.id UUID string.
	ID string `json:"id"`
	// Email is the user's email address.
	Email string `json:"email"`
	// EmailConfirmedAt is non-nil when the user's email has been confirmed.
	// A nil value means the user has not yet confirmed their email.
	EmailConfirmedAt *time.Time `json:"email_confirmed_at"`
}

// SupabaseClient abstracts the Supabase Auth user endpoint.
// The interface allows the /auth/exchange handler to be tested with a mock.
type SupabaseClient interface {
	// GetUser calls GET /auth/v1/user with the given access token.
	// Returns the user on success, or an error when Supabase responds non-2xx
	// or the response cannot be decoded.
	GetUser(ctx context.Context, accessToken string) (*SupabaseUser, error)
}

// supabaseHTTPClient is the production implementation of SupabaseClient.
// It hits the real Supabase Auth REST API.
type supabaseHTTPClient struct {
	// projectURL is the base URL: https://<ref>.supabase.co
	projectURL string
	// anonKey is the public Supabase anon key (sent as apikey header).
	anonKey string
	// client is the underlying HTTP client.
	client *http.Client
}

// NewSupabaseClient returns a SupabaseClient backed by a real HTTP call.
// projectURL must be the Supabase project base URL (no trailing slash).
// anonKey is the Supabase anon JWT.
func NewSupabaseClient(projectURL, anonKey string) SupabaseClient {
	return &supabaseHTTPClient{
		projectURL: projectURL,
		anonKey:    anonKey,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// supabaseEndpoint is the path used in all external_supabase_* log events.
const supabaseEndpoint = "/auth/v1/user"

// supabaseResponseBodyCap is the maximum bytes read from a Supabase response body
// before logging. Prevents memory blowup on unexpectedly large responses.
const supabaseResponseBodyCap = 8192

// GetUser calls GET <projectURL>/auth/v1/user with the bearer access token.
// Returns ErrSupabaseUnauthorized when Supabase responds 401/403; any other
// non-2xx response or decode failure returns a descriptive error.
//
// Emits three structured log events for observability (cache misses only — cache
// hits never reach this method):
//   - external_supabase_request  (INFO)  — before the HTTP call
//   - external_supabase_response (INFO)  — on 2xx with the decoded body
//   - external_supabase_failed   (ERROR) — on network error, non-2xx, or decode failure
func (c *supabaseHTTPClient) GetUser(ctx context.Context, accessToken string) (*SupabaseUser, error) {
	url := c.projectURL + supabaseEndpoint

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("apikey", c.anonKey)

	// AC-ES.1: emit request log before the HTTP call so timestamp always precedes
	// the response log, regardless of how long the network round-trip takes.
	slog.InfoContext(ctx, "external_supabase_request",
		"service", "supabase",
		"operation", "request",
		"method", "GET",
		"endpoint", supabaseEndpoint,
		"body", "GET "+supabaseEndpoint,
	)
	start := time.Now()

	resp, err := c.client.Do(req)
	if err != nil {
		// AC-ES.3: network-level failure (DNS, connection refused, timeout …).
		slog.ErrorContext(ctx, "external_supabase_failed",
			"service", "supabase",
			"operation", "error",
			"endpoint", supabaseEndpoint,
			"duration_ms", time.Since(start).Milliseconds(),
			"body", err.Error(),
		)
		return nil, fmt.Errorf("supabase: GET /auth/v1/user: %w", err)
	}
	defer resp.Body.Close()

	// Read the full body once so we can log it and still decode it below.
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, supabaseResponseBodyCap))
	// Restore the body for the JSON decoder further down.
	resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// AC-ES.4: non-2xx (auth failure) — log as failed with status code in body.
		slog.ErrorContext(ctx, "external_supabase_failed",
			"service", "supabase",
			"operation", "error",
			"endpoint", supabaseEndpoint,
			"status_code", resp.StatusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"body", string(bodyBytes),
		)
		return nil, ErrSupabaseUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// AC-ES.4: any other non-2xx — log as failed with body from Supabase.
		slog.ErrorContext(ctx, "external_supabase_failed",
			"service", "supabase",
			"operation", "error",
			"endpoint", supabaseEndpoint,
			"status_code", resp.StatusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"body", string(bodyBytes),
		)
		return nil, fmt.Errorf("supabase: GET /auth/v1/user returned %d: %s", resp.StatusCode, bodyBytes)
	}

	var user SupabaseUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		// Decode failure after a 2xx — still a failed call from our perspective.
		slog.ErrorContext(ctx, "external_supabase_failed",
			"service", "supabase",
			"operation", "error",
			"endpoint", supabaseEndpoint,
			"status_code", resp.StatusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"body", fmt.Sprintf("decode error: %s; raw body: %s", err.Error(), string(bodyBytes)),
		)
		return nil, fmt.Errorf("supabase: decode user response: %w", err)
	}

	// AC-ES.2: successful 2xx — log with the raw response body.
	slog.InfoContext(ctx, "external_supabase_response",
		"service", "supabase",
		"operation", "response",
		"endpoint", supabaseEndpoint,
		"status_code", resp.StatusCode,
		"duration_ms", time.Since(start).Milliseconds(),
		"body", string(bodyBytes),
	)
	return &user, nil
}

// ErrSupabaseUnauthorized is returned by GetUser when Supabase responds with
// 401 or 403, indicating the access token is invalid or expired.
// The /auth/exchange handler maps this to auth/invalid-supabase-token (401).
var ErrSupabaseUnauthorized = fmt.Errorf("supabase: access token rejected (401/403)")
