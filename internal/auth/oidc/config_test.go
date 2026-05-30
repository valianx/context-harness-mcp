package oidc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadOIDCConfig_Defaults(t *testing.T) {
	t.Setenv("OIDC_ISSUER_URL", "https://accounts.google.com")
	t.Setenv("OIDC_CLIENT_ID", "client-id")
	t.Setenv("OIDC_CLIENT_SECRET", "secret")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")
	t.Setenv("OIDC_SCOPES", "")
	t.Setenv("OIDC_ID_TOKEN_SIGNING_ALGS", "")
	t.Setenv("OIDC_REDIRECT_URL", "")

	cfg := LoadOIDCConfig()

	assert.Equal(t, "https://accounts.google.com", cfg.IssuerURL)
	assert.Equal(t, "client-id", cfg.ClientID)
	assert.Equal(t, "https://mcp.example.com/auth/callback", cfg.RedirectURL)
	assert.Equal(t, []string{"openid", "email", "profile"}, cfg.Scopes)
	assert.Equal(t, []string{"RS256"}, cfg.SupportedSigningAlgs)
}

func TestLoadOIDCConfig_ExplicitRedirectURL(t *testing.T) {
	t.Setenv("OIDC_REDIRECT_URL", "https://custom.example.com/auth/callback")
	t.Setenv("MCP_PUBLIC_URL", "https://mcp.example.com")

	cfg := LoadOIDCConfig()

	// Explicit OIDC_REDIRECT_URL takes precedence.
	assert.Equal(t, "https://custom.example.com/auth/callback", cfg.RedirectURL)
}

func TestLoadOIDCConfig_CustomScopes(t *testing.T) {
	t.Setenv("OIDC_SCOPES", "openid email")
	t.Setenv("OIDC_ID_TOKEN_SIGNING_ALGS", "RS256,ES256")

	cfg := LoadOIDCConfig()

	assert.Equal(t, []string{"openid", "email"}, cfg.Scopes)
	assert.Equal(t, []string{"RS256", "ES256"}, cfg.SupportedSigningAlgs)
}

func TestOIDCConfig_Validate_RequiredFields(t *testing.T) {
	base := OIDCConfig{
		IssuerURL:            "https://accounts.google.com",
		ClientID:             "id",
		ClientSecret:         "secret",
		RedirectURL:          "https://mcp.example.com/auth/callback",
		SupportedSigningAlgs: []string{"RS256"},
	}

	tests := []struct {
		name    string
		mutate  func(*OIDCConfig)
		wantErr string
	}{
		{
			name:    "valid",
			mutate:  func(_ *OIDCConfig) {},
			wantErr: "",
		},
		{
			name:    "missing_issuer",
			mutate:  func(c *OIDCConfig) { c.IssuerURL = "" },
			wantErr: "OIDC_ISSUER_URL",
		},
		{
			name:    "missing_client_id",
			mutate:  func(c *OIDCConfig) { c.ClientID = "" },
			wantErr: "OIDC_CLIENT_ID",
		},
		{
			name:    "missing_client_secret",
			mutate:  func(c *OIDCConfig) { c.ClientSecret = "" },
			wantErr: "OIDC_CLIENT_SECRET",
		},
		{
			name:    "missing_redirect_url",
			mutate:  func(c *OIDCConfig) { c.RedirectURL = "" },
			wantErr: "OIDC_REDIRECT_URL",
		},
		{
			name:    "http_redirect_non_localhost",
			mutate:  func(c *OIDCConfig) { c.RedirectURL = "http://prod.example.com/auth/callback" },
			wantErr: "https://",
		},
		{
			name:    "http_redirect_localhost_allowed",
			mutate:  func(c *OIDCConfig) { c.RedirectURL = "http://localhost:7654/auth/callback" },
			wantErr: "",
		},
		{
			name:    "empty_signing_algs",
			mutate:  func(c *OIDCConfig) { c.SupportedSigningAlgs = nil },
			wantErr: "OIDC_ID_TOKEN_SIGNING_ALGS",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestIsConfigured(t *testing.T) {
	t.Setenv("OIDC_ISSUER_URL", "")
	assert.False(t, IsConfigured())

	t.Setenv("OIDC_ISSUER_URL", "https://accounts.google.com")
	assert.True(t, IsConfigured())
}

// TestIsLocalhost_ExactMatch verifies that isLocalhost uses exact hostname
// comparison and rejects URLs whose hostname merely contains "localhost" or
// "127.0.0.1" as a substring (fix: SEC-003, CWE-697).
func TestIsLocalhost_ExactMatch(t *testing.T) {
	tests := []struct {
		url     string
		want    bool
		comment string
	}{
		// True cases — genuine loopback addresses.
		{"http://localhost/auth/callback", true, "plain localhost"},
		{"http://localhost:7654/auth/callback", true, "localhost with port"},
		{"https://localhost/auth/callback", true, "localhost over https"},
		{"http://127.0.0.1/auth/callback", true, "loopback IPv4"},
		{"http://127.0.0.1:8080/auth/callback", true, "loopback IPv4 with port"},
		{"http://[::1]/auth/callback", true, "loopback IPv6"},

		// False cases — hostnames that CONTAIN "localhost" but are not loopback.
		// These must NOT be treated as local (the pre-fix strings.Contains bug).
		{"http://localhost.evil.com/auth/callback", false, "subdomain of localhost"},
		{"https://localhost.evil.com/auth/callback", false, "subdomain of localhost over https"},
		{"http://127.0.0.1.attacker.io/auth/callback", false, "127.0.0.1 prefix in hostname"},
		{"http://notlocalhost/auth/callback", false, "hostname ending in localhost"},
		{"https://evil.com/?x=localhost", false, "localhost in query string only"},

		// False cases — legitimate remote hosts.
		{"https://accounts.google.com/auth/callback", false, "google"},
		{"https://mcp.example.com/auth/callback", false, "prod deployment"},
	}

	for _, tc := range tests {
		t.Run(tc.comment, func(t *testing.T) {
			got := isLocalhost(tc.url)
			assert.Equal(t, tc.want, got,
				"isLocalhost(%q) = %v, want %v (%s)", tc.url, got, tc.want, tc.comment)
		})
	}
}

// TestOIDCConfig_Validate_LocalhostBypassPrevented ensures that the HTTPS
// enforcement in Validate() cannot be bypassed via a hostname that contains
// "localhost" as a substring (e.g. "localhost.evil.com").
func TestOIDCConfig_Validate_LocalhostBypassPrevented(t *testing.T) {
	base := OIDCConfig{
		IssuerURL:            "https://accounts.google.com",
		ClientID:             "id",
		ClientSecret:         "secret",
		SupportedSigningAlgs: []string{"RS256"},
	}

	bypassAttempts := []string{
		"http://localhost.evil.com/auth/callback",
		"http://127.0.0.1.attacker.io/auth/callback",
	}

	for _, redirectURL := range bypassAttempts {
		t.Run(redirectURL, func(t *testing.T) {
			cfg := base
			cfg.RedirectURL = redirectURL
			err := cfg.Validate()
			require.Error(t, err,
				"Validate() must reject %q — it is not a localhost URL", redirectURL)
			assert.Contains(t, err.Error(), "https://",
				"error must mention the https requirement")
		})
	}
}
