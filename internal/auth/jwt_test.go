package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testSecret  = "test-jwt-secret-32-bytes-minimum!!"
	testSub     = "550e8400-e29b-41d4-a716-446655440000"
	testEmail   = "dev@example.com"
	testIssuer  = "context-harness-mcp"
)

// setupJWTEnv sets the required environment variables for JWT operations and
// returns a cleanup function that restores the original values.
func setupJWTEnv(t *testing.T) {
	t.Helper()
	t.Setenv("MCP_JWT_SECRET", testSecret)
	t.Setenv("MCP_JWT_ISSUER", testIssuer)
}

// ── AC-1: valid token roundtrip ───────────────────────────────────────────────

// TestIssueMCPToken_ValidRoundtrip verifies that a token issued by IssueMCPToken
// can be validated by ValidateMCPToken and that the returned Claims carry the
// correct sub and email.
//
// AC-1: Given valid HS256-signed JWT, When ValidateMCPToken runs, Then returns Claims with error == nil.
func TestIssueMCPToken_ValidRoundtrip(t *testing.T) {
	setupJWTEnv(t)

	tokenStr, err := IssueMCPToken(testSub, testEmail)
	require.NoError(t, err, "IssueMCPToken must succeed when MCP_JWT_SECRET is set")
	require.NotEmpty(t, tokenStr, "issued token must be non-empty")

	claims, err := ValidateMCPToken(tokenStr)
	require.NoError(t, err, "ValidateMCPToken must accept the token it just issued")
	require.NotNil(t, claims)

	assert.Equal(t, testSub, claims.Subject, "Subject must match the issued sub")
	assert.Equal(t, testEmail, claims.Email, "Email must match the issued email")
	assert.Equal(t, testIssuer, claims.Issuer, "Issuer must match MCP_JWT_ISSUER")
	assert.NotNil(t, claims.IssuedAt, "iat must be set")
	assert.NotNil(t, claims.ExpiresAt, "exp must be set")
	assert.True(t, claims.ExpiresAt.After(time.Now()), "exp must be in the future")
}

// TestIssueMCPToken_MissingSecret verifies that IssueMCPToken returns an error
// when MCP_JWT_SECRET is absent.
func TestIssueMCPToken_MissingSecret(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", "")

	_, err := IssueMCPToken(testSub, testEmail)
	require.Error(t, err, "IssueMCPToken must fail when MCP_JWT_SECRET is not set")
	assert.Contains(t, err.Error(), "MCP_JWT_SECRET", "error must mention the missing var")
}

// TestValidateMCPToken_MissingSecret verifies that ValidateMCPToken returns an
// error when MCP_JWT_SECRET is absent (no token can be validated without secret).
func TestValidateMCPToken_MissingSecret(t *testing.T) {
	t.Setenv("MCP_JWT_SECRET", "")

	_, err := ValidateMCPToken("any.token.here")
	require.Error(t, err, "ValidateMCPToken must fail when MCP_JWT_SECRET is not set")
}

// ── AC-2: algorithm-confusion prevention ──────────────────────────────────────

// TestValidateMCPToken_AlgNone verifies that a token with alg=none is rejected
// with an error that maps to auth/invalid-token.
//
// AC-2: Given JWT with alg:none, When ValidateMCPToken runs, Then returns auth/invalid-token.
func TestValidateMCPToken_AlgNone(t *testing.T) {
	setupJWTEnv(t)

	// Build a token manually with alg:none. golang-jwt refuses to sign these,
	// so we craft the header+payload+empty-signature by hand.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + testSub + `","iss":"context-harness-mcp","iat":1000000,"exp":9999999999}`))
	algNoneToken := header + "." + payload + "."

	_, err := ValidateMCPToken(algNoneToken)
	require.Error(t, err, "alg:none token must be rejected")
	assert.Equal(t, CodeInvalidToken, ClassifyJWTError(err),
		"alg:none must classify as auth/invalid-token")
}

// TestValidateMCPToken_AlgRS256 verifies that a token signed with RS256 is
// rejected even if the signature were somehow valid — only HS256 is accepted.
//
// AC-2: Given JWT with alg:RS256, When ValidateMCPToken runs, Then returns auth/invalid-token.
func TestValidateMCPToken_AlgRS256(t *testing.T) {
	setupJWTEnv(t)

	// Craft an RS256-header token with a bogus signature. The library will
	// reject it at alg validation before even checking the signature.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"` + testSub + `","iss":"context-harness-mcp","iat":1000000,"exp":9999999999}`))
	rs256Token := header + "." + payload + ".bogus-signature"

	_, err := ValidateMCPToken(rs256Token)
	require.Error(t, err, "RS256 token must be rejected")
	assert.Equal(t, CodeInvalidToken, ClassifyJWTError(err),
		"RS256 must classify as auth/invalid-token")
}

// ── AC-3: expired token returns auth/expired ─────────────────────────────────

// TestValidateMCPToken_Expired verifies that a token with exp 1 second in the
// past is rejected with the auth/expired code. No leeway window.
//
// AC-3: Given JWT with exp 1s in the past, When ValidateMCPToken runs, Then returns auth/expired.
func TestValidateMCPToken_Expired(t *testing.T) {
	setupJWTEnv(t)

	secret := []byte(testSecret)
	now := time.Now()

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   testSub,
			Issuer:    testIssuer,
			IssuedAt:  jwt.NewNumericDate(now.Add(-2 * time.Second)),
			ExpiresAt: jwt.NewNumericDate(now.Add(-1 * time.Second)), // 1s in the past
		},
		Email: testEmail,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString(secret)
	require.NoError(t, err, "must be able to sign expired token for test")

	_, err = ValidateMCPToken(tokenStr)
	require.Error(t, err, "expired token must be rejected")
	assert.Equal(t, CodeExpired, ClassifyJWTError(err),
		"expired token must classify as auth/expired")
}

// ── AC-4: tampered signature returns auth/invalid-token ───────────────────────

// TestValidateMCPToken_TamperedSignature verifies that a token whose signed
// content (payload) is mutated post-signature is rejected as auth/invalid-token.
//
// AC-4: Given JWT with signed content tampered, When ValidateMCPToken runs,
// Then returns auth/invalid-token.
//
// Note: we mutate the payload's first byte (not the signature's trailing byte),
// because the last char of a base64url-encoded HS256 signature carries padding
// bits — flipping bit 0 there can decode to the same raw signature bytes.
// Mutating the payload guarantees the signature no longer matches the content.
func TestValidateMCPToken_TamperedSignature(t *testing.T) {
	setupJWTEnv(t)

	tokenStr, err := IssueMCPToken(testSub, testEmail)
	require.NoError(t, err)

	// JWT compact form: header.payload.signature. Flip bit 0 of the first
	// base64url char of the payload — guaranteed to corrupt the signed content
	// without depending on base64 padding-bit positions.
	parts := strings.Split(tokenStr, ".")
	require.Len(t, parts, 3, "JWT must have 3 parts")

	payload := []byte(parts[1])
	payload[0] ^= 0x01
	tampered := parts[0] + "." + string(payload) + "." + parts[2]

	_, err = ValidateMCPToken(tampered)
	require.Error(t, err, "token with tampered payload must be rejected")
	assert.Equal(t, CodeInvalidToken, ClassifyJWTError(err),
		"tampered content must classify as auth/invalid-token")
}

// ── helper: custom expiry via env ──────────────────────────────────────────────

// TestJwtExpiry_CustomDuration verifies that MCP_JWT_EXPIRY is respected.
func TestJwtExpiry_CustomDuration(t *testing.T) {
	setupJWTEnv(t)
	t.Setenv("MCP_JWT_EXPIRY", "2h")

	tokenStr, err := IssueMCPToken(testSub, testEmail)
	require.NoError(t, err)

	claims, err := ValidateMCPToken(tokenStr)
	require.NoError(t, err)

	expectedExpiry := time.Now().Add(2 * time.Hour)
	// Allow 5 seconds of tolerance for test execution time.
	diff := claims.ExpiresAt.Time.Sub(expectedExpiry)
	if diff < 0 {
		diff = -diff
	}
	assert.Less(t, diff.Seconds(), float64(5),
		"ExpiresAt must be approximately now+2h (custom expiry)")
}

// TestJwtIssuer_IssuerMismatch verifies that a token with a mismatched iss
// is rejected. ValidateMCPToken checks iss manually after signature verification.
func TestJwtIssuer_IssuerMismatch(t *testing.T) {
	setupJWTEnv(t)

	secret := []byte(testSecret)
	now := time.Now()

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   testSub,
			Issuer:    "wrong-issuer", // intentional mismatch
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		Email: testEmail,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString(secret)
	require.NoError(t, err)

	_, err = ValidateMCPToken(tokenStr)
	require.Error(t, err, "token with wrong issuer must be rejected")
	// Issuer mismatch is not an expiry error — maps to invalid-token.
	assert.Equal(t, CodeInvalidToken, ClassifyJWTError(err),
		"issuer mismatch must classify as auth/invalid-token")
}

// TestValidateMCPToken_Malformed verifies that a completely malformed string
// (not even parseable as JWT) is rejected as invalid-token.
func TestValidateMCPToken_Malformed(t *testing.T) {
	setupJWTEnv(t)

	_, err := ValidateMCPToken("this.is.not.a.jwt")
	require.Error(t, err, "malformed token must be rejected")
	assert.Equal(t, CodeInvalidToken, ClassifyJWTError(err))
}
