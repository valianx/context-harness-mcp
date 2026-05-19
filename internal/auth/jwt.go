package auth

import (
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// defaultIssuer is the iss claim value when MCP_JWT_ISSUER is not set.
	defaultIssuer = "context-harness-mcp"
	// defaultExpiry is the token lifetime when MCP_JWT_EXPIRY is not set.
	// 1 year matches the locked decision for bearer tokens in ~/.claude.json.
	defaultExpiry = 8760 * time.Hour
)

// Claims holds the custom JWT payload for MCP bearer tokens.
// Only HS256-signed tokens with these claims are accepted by ValidateMCPToken.
type Claims struct {
	jwt.RegisteredClaims
	// Email is the user's email at issue time — for audit/UX only.
	// The DB row (users.revoked_at) is the authoritative source.
	Email string `json:"email"`
}

// IssueMCPToken signs a new MCP JWT for the given Supabase user.
// sub must be a valid UUID string (Supabase auth.users.id).
// Returns a compact JWT string or an error if the secret is absent/invalid.
func IssueMCPToken(sub, email string) (string, error) {
	secret, err := jwtSecret()
	if err != nil {
		return "", err
	}

	expiry := jwtExpiry()
	issuer := jwtIssuer()

	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   sub,
			Issuer:    issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(expiry)),
		},
		Email: email,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(secret)
}

// ValidateMCPToken parses and validates a compact JWT string.
// It enforces HS256-only (algorithm-confusion prevention), issuer, and expiry.
// Returns the parsed Claims on success, or an error whose code can be mapped
// with ClassifyJWTError.
func ValidateMCPToken(tokenString string) (*Claims, error) {
	secret, err := jwtSecret()
	if err != nil {
		return nil, err
	}

	issuer := jwtIssuer()

	// Parse with signature-only validation first. We pass WithoutClaimsValidation
	// so the library validates signature + alg + iss, and we then do expiry
	// manually so we can distinguish jwt.ErrTokenExpired from jwt.ErrTokenSignatureInvalid
	// in the middleware error classifier (AC-3 vs AC-2/AC-4).
	//
	// Note: WithIssuer still works without WithoutClaimsValidation because iss
	// is validated by the RegisteredClaims.Valid() call inside ParseWithClaims.
	// We explicitly separate the two phases to get clean error classification.
	var claims Claims
	token, err := jwt.ParseWithClaims(tokenString, &claims,
		func(t *jwt.Token) (any, error) {
			return secret, nil
		},
		// Reject alg:none, RS256, and any algorithm other than HS256.
		// This is the primary defense against algorithm-confusion attacks.
		jwt.WithValidMethods([]string{"HS256"}),
		// Skip built-in claims validation so we can do manual expiry check below.
		// Issuer is validated manually in validateClaims.
		jwt.WithoutClaimsValidation(),
	)
	if err != nil {
		return nil, err
	}
	_ = token // token.Valid is not checked; we rely on err above for sig failure

	// Manual claims validation after signature verification lets us distinguish
	// expired (auth/expired) from bad-signature (auth/invalid-token).
	if err := validateClaims(&claims, issuer); err != nil {
		return nil, err
	}

	return &claims, nil
}

// validateClaims checks expiry and issuer manually so ValidateMCPToken can
// distinguish between an expired token (auth/expired) and other validation
// failures (auth/invalid-token). No leeway window per locked decision.
func validateClaims(c *Claims, expectedIssuer string) error {
	now := time.Now()
	if c.ExpiresAt == nil || c.ExpiresAt.Time.Before(now) {
		return jwt.ErrTokenExpired
	}
	if c.Subject == "" {
		return fmt.Errorf("missing sub claim")
	}
	if c.Issuer != expectedIssuer {
		return fmt.Errorf("issuer mismatch: got %q, want %q", c.Issuer, expectedIssuer)
	}
	return nil
}

// jwtSecret reads MCP_JWT_SECRET from the environment and returns it as bytes.
// Returns an error when the variable is absent — callers must not issue or
// validate tokens without a configured secret.
func jwtSecret() ([]byte, error) {
	s := os.Getenv("MCP_JWT_SECRET")
	if s == "" {
		return nil, fmt.Errorf("MCP_JWT_SECRET is not set")
	}
	return []byte(s), nil
}

// jwtIssuer returns the configured issuer or the default.
func jwtIssuer() string {
	if v := os.Getenv("MCP_JWT_ISSUER"); v != "" {
		return v
	}
	return defaultIssuer
}

// jwtExpiry parses MCP_JWT_EXPIRY or returns the default 1-year duration.
func jwtExpiry() time.Duration {
	raw := os.Getenv("MCP_JWT_EXPIRY")
	if raw == "" {
		return defaultExpiry
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return defaultExpiry
	}
	return d
}
