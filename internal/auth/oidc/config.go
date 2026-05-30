// Package oidc implements provider-agnostic OIDC login (Authorization Code + PKCE).
// It encapsulates discovery, token exchange, ID-token verification, and the
// ephemeral state/nonce/PKCE cookie — all security-critical surfaces audited
// against the 11-threat checklist in 01-plan.md §Security Assessment.
package oidc

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// OIDCConfig holds the provider configuration sourced from environment variables.
// All fields map 1:1 to the OIDC_* env vars documented in docs/auth.md.
type OIDCConfig struct {
	// IssuerURL is the OIDC issuer base URL (e.g. https://accounts.google.com).
	// go-oidc appends /.well-known/openid-configuration automatically.
	IssuerURL string

	// ClientID is the OAuth2 client ID registered with the provider.
	// It is also the expected audience (aud) of ID tokens.
	ClientID string

	// ClientSecret is the OAuth2 client secret. Confidential-client flow only.
	// Never embedded in HTML or logged.
	ClientSecret string

	// RedirectURL is the callback URL registered with the provider.
	// Derived from MCP_PUBLIC_URL when absent.
	RedirectURL string

	// Scopes is the space-separated list of OAuth2 scopes to request.
	// "openid" is always included; "email" and "profile" are defaults.
	Scopes []string

	// SupportedSigningAlgs pins the ID-token signing algorithms accepted
	// by the verifier. Prevents algorithm confusion (alg:none, HS256 swap).
	SupportedSigningAlgs []string
}

// LoadOIDCConfig reads configuration from the environment.
// Returns an OIDCConfig with defaults applied for optional fields.
func LoadOIDCConfig() OIDCConfig {
	redirectURL := os.Getenv("OIDC_REDIRECT_URL")
	if redirectURL == "" {
		publicURL := strings.TrimRight(os.Getenv("MCP_PUBLIC_URL"), "/")
		if publicURL != "" {
			redirectURL = publicURL + "/auth/callback"
		}
	}

	scopes := parseSpaceSeparated(os.Getenv("OIDC_SCOPES"), []string{"openid", "email", "profile"})

	algs := parseCommaSeparated(os.Getenv("OIDC_ID_TOKEN_SIGNING_ALGS"), []string{"RS256"})

	return OIDCConfig{
		IssuerURL:            os.Getenv("OIDC_ISSUER_URL"),
		ClientID:             os.Getenv("OIDC_CLIENT_ID"),
		ClientSecret:         os.Getenv("OIDC_CLIENT_SECRET"),
		RedirectURL:          redirectURL,
		Scopes:               scopes,
		SupportedSigningAlgs: algs,
	}
}

// IsConfigured returns true when the minimum OIDC_* variables are present,
// indicating the server should use the OIDC flow rather than Supabase legacy.
func IsConfigured() bool {
	return os.Getenv("OIDC_ISSUER_URL") != ""
}

// Validate checks that all required fields are present and safe for an
// auth-enabled deployment. Returns an error with a descriptive message so the
// server can fail fast at boot.
//
// Security (threat #9 — CWE-319): redirect_uri must be HTTPS for non-localhost
// deployments to prevent token interception in transit.
func (c OIDCConfig) Validate() error {
	if c.IssuerURL == "" {
		return fmt.Errorf("OIDC_ISSUER_URL is required when OIDC mode is active")
	}
	if c.ClientID == "" {
		return fmt.Errorf("OIDC_CLIENT_ID is required when OIDC mode is active")
	}
	if c.ClientSecret == "" {
		return fmt.Errorf("OIDC_CLIENT_SECRET is required when OIDC mode is active")
	}
	if c.RedirectURL == "" {
		return fmt.Errorf("OIDC_REDIRECT_URL or MCP_PUBLIC_URL is required to derive the callback URL")
	}

	// Enforce HTTPS for non-localhost redirect URIs (threat #9).
	if !isLocalhost(c.RedirectURL) && !strings.HasPrefix(c.RedirectURL, "https://") {
		return fmt.Errorf("OIDC_REDIRECT_URL must use https:// for non-localhost deployments (got %q)", c.RedirectURL)
	}

	if len(c.SupportedSigningAlgs) == 0 {
		return fmt.Errorf("OIDC_ID_TOKEN_SIGNING_ALGS must not be empty")
	}

	return nil
}

// isLocalhost returns true when the URL's hostname is exactly "localhost",
// "127.0.0.1", or "::1", allowing plain http:// redirect URIs in local
// development. Uses exact hostname comparison (not substring) to prevent
// bypass via hostnames such as "localhost.evil.com" (fix: SEC-003, CWE-697).
func isLocalhost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		// Unparseable URL is not a localhost URL.
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// parseSpaceSeparated splits a space-separated string into a slice.
// Returns defaults when the input is empty.
func parseSpaceSeparated(s string, defaults []string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaults
	}
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return defaults
	}
	return parts
}

// parseCommaSeparated splits a comma-separated string into a slice.
// Returns defaults when the input is empty.
func parseCommaSeparated(s string, defaults []string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaults
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	if len(result) == 0 {
		return defaults
	}
	return result
}
