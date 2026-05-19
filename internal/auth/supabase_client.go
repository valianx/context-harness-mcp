package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// GetUser calls GET <projectURL>/auth/v1/user with the bearer access token.
// Returns ErrSupabaseUnauthorized when Supabase responds 401/403; any other
// non-2xx response or decode failure returns a descriptive error.
func (c *supabaseHTTPClient) GetUser(ctx context.Context, accessToken string) (*SupabaseUser, error) {
	url := c.projectURL + "/auth/v1/user"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("supabase: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("apikey", c.anonKey)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supabase: GET /auth/v1/user: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrSupabaseUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("supabase: GET /auth/v1/user returned %d: %s", resp.StatusCode, body)
	}

	var user SupabaseUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("supabase: decode user response: %w", err)
	}
	return &user, nil
}

// ErrSupabaseUnauthorized is returned by GetUser when Supabase responds with
// 401 or 403, indicating the access token is invalid or expired.
// The /auth/exchange handler maps this to auth/invalid-supabase-token (401).
var ErrSupabaseUnauthorized = fmt.Errorf("supabase: access token rejected (401/403)")
