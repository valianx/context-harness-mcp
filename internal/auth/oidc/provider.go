package oidc

import (
	"context"
	"fmt"
	"sync"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Provider wraps the go-oidc provider, oauth2 config, and ID-token verifier.
// It is long-lived (created once at first use via sync.Once) to benefit from
// JWKS caching in the go-oidc remote key set.
//
// Security: the Verifier is built with SupportedSigningAlgs from config so
// alg:none and symmetric algorithms are rejected (threats #5, #6 — CWE-347, CWE-287).
// InsecureSkipSignatureCheck is NEVER set.
type Provider struct {
	oidcProvider *gooidc.Provider
	oauth2Config oauth2.Config
	verifier     *gooidc.IDTokenVerifier
}

// providerSingleton holds the lazily-initialized provider.
var (
	providerOnce     sync.Once
	providerInstance *Provider
	providerErr      error
)

// NewProvider returns the lazily-initialized OIDC provider using config loaded
// from the environment. Discovery runs on the first call; subsequent calls
// return the cached instance immediately.
//
// Discovery is lazy (not at main() boot) so the server starts even when the
// IdP is temporarily unavailable — only the login path fails until recovery
// (threat #7 — JWKS / discovery resilience).
func NewProvider(ctx context.Context) (*Provider, error) {
	providerOnce.Do(func() {
		providerInstance, providerErr = newProvider(ctx)
	})
	return providerInstance, providerErr
}

// newProvider performs OIDC discovery and builds the Provider.
// Called exactly once via sync.Once.
func newProvider(ctx context.Context) (*Provider, error) {
	cfg := LoadOIDCConfig()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("oidc: config invalid: %w", err)
	}

	// Discovery: go-oidc fetches /.well-known/openid-configuration and caches it.
	raw, err := gooidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery failed for %s: %w", cfg.IssuerURL, err)
	}

	// Verifier: built once per provider lifetime.
	// - ClientID sets the expected audience (aud claim) — threat #6.
	// - SupportedSigningAlgs pins acceptable algorithms — threat #5.
	// - SkipClientIDCheck / SkipExpiryCheck / SkipIssuerCheck remain false (default).
	// - InsecureSkipSignatureCheck is NOT set — signature validation is always on.
	verifierCfg := &gooidc.Config{
		ClientID:             cfg.ClientID,
		SupportedSigningAlgs: cfg.SupportedSigningAlgs,
	}
	verifier := raw.Verifier(verifierCfg)

	o2cfg := oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     raw.Endpoint(),
		RedirectURL:  cfg.RedirectURL,
		Scopes:       cfg.Scopes,
	}

	return &Provider{
		oidcProvider: raw,
		oauth2Config: o2cfg,
		verifier:     verifier,
	}, nil
}

// resetForTest clears the singleton so tests can initialize with a custom issuer.
// Must be called only from test code.
func resetForTest() {
	providerOnce = sync.Once{}
	providerInstance = nil
	providerErr = nil
}

