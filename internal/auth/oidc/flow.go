package oidc

import (
	"context"
	"fmt"

	"golang.org/x/oauth2"
)

// Identity holds the verified claims extracted from a successful OIDC exchange.
// Sub is the provider-issued subject identifier (stable per user).
// Email is the user's email at time of login.
// Nonce is the nonce from the ID token — the caller MUST compare this against
// the value stored in the ephemeral state cookie before trusting any other field.
type Identity struct {
	Sub   string
	Email string
	Nonce string
}

// idTokenClaims is the minimal claim set extracted from the ID token.
type idTokenClaims struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
}

// AuthCodeURL generates the authorization endpoint URL for the provider.
// It embeds the PKCE code_challenge (S256), state, and nonce parameters.
//
// Security:
//   - PKCE: S256 challenge derived from verifier (threat #1 — CWE-639).
//   - state: passed as the OAuth2 state parameter (threat #2 — CWE-352).
//   - nonce: passed via SetAuthURLParam so the provider includes it in the ID token
//     (threat #3 — CWE-294). go-oidc does NOT validate nonce — the caller must.
func (p *Provider) AuthCodeURL(state, nonce, pkceVerifier string) string {
	return p.oauth2Config.AuthCodeURL(
		state,
		oauth2.S256ChallengeOption(pkceVerifier),
		oauth2.SetAuthURLParam("nonce", nonce),
	)
}

// Exchange performs the authorization code exchange and verifies the ID token.
// It returns an Identity with the verified sub, email, and nonce.
//
// The caller MUST check Identity.Nonce against the value from the ephemeral
// cookie before proceeding (threat #3 — nonce replay protection).
//
// Security:
//   - PKCE verifier is sent with the token request (threat #1).
//   - verifier.Verify validates signature (against JWKS), iss, aud, exp (threats #4, #5, #6).
//   - alg:none is rejected by go-oidc by default + SupportedSigningAlgs pin (threat #5).
//   - The raw ID token and authorization code are never logged.
func (p *Provider) Exchange(ctx context.Context, code, pkceVerifier string) (*Identity, error) {
	// Exchange authorization code for tokens. PKCE verifier included (threat #1).
	token, err := p.oauth2Config.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return nil, fmt.Errorf("oidc: token exchange failed: %w", err)
	}

	// Extract the raw ID token from the token response.
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, fmt.Errorf("oidc: id_token missing from token response")
	}

	// Verify the ID token: signature (JWKS), iss, aud, exp, alg pin.
	// This is the primary security gate — any failure rejects the login.
	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("oidc: id_token verification failed: %w", err)
	}

	// Extract standard claims from the verified token.
	var claims idTokenClaims
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("oidc: claims extraction failed: %w", err)
	}

	if claims.Sub == "" {
		return nil, fmt.Errorf("oidc: id_token missing sub claim")
	}

	// idToken.Nonce is the nonce embedded in the ID token by the provider.
	// The caller validates it against the cookie-stored nonce (threat #3).
	return &Identity{
		Sub:   claims.Sub,
		Email: claims.Email,
		Nonce: idToken.Nonce,
	}, nil
}
