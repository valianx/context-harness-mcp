package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testBaseURL = "https://test"

// errorResponse is the decoded JSON body returned by WriteError.
type errorResponse struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	AuthLoginURL string `json:"auth_login_url"`
	Layer        string `json:"layer"`
}

// captureWriteError calls WriteError and returns the recorded response.
func captureWriteError(code, baseURL string) (*httptest.ResponseRecorder, errorResponse) {
	w := httptest.NewRecorder()
	WriteError(w, code, baseURL)

	var body errorResponse
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w, body
}

// ── AC-9: structured error response shape for each code ──────────────────────

// TestWriteError_Unauthenticated verifies the shape, status, content-type, and
// Spanish message for auth/unauthenticated.
//
// AC-9: Given code auth/unauthenticated, When WriteError runs, Then body matches exact shape.
func TestWriteError_Unauthenticated(t *testing.T) {
	w, body := captureWriteError(CodeUnauthenticated, testBaseURL)

	assert.Equal(t, http.StatusUnauthorized, w.Code, "status must be 401")
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"), "content-type must be application/json")

	assert.Equal(t, CodeUnauthenticated, body.Code)
	assert.Equal(t, "auth", body.Layer)
	assert.Equal(t, testBaseURL+"/auth/login", body.AuthLoginURL,
		"auth_login_url must be baseURL+/auth/login")
	assert.Contains(t, body.Message, "Falta el header Authorization Bearer",
		"message must contain Spanish text for unauthenticated")
	assert.Contains(t, body.Message, testBaseURL+"/auth/login",
		"message must include the login URL")
}

// TestWriteError_InvalidToken verifies the shape for auth/invalid-token.
func TestWriteError_InvalidToken(t *testing.T) {
	w, body := captureWriteError(CodeInvalidToken, testBaseURL)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, CodeInvalidToken, body.Code)
	assert.Equal(t, "auth", body.Layer)
	assert.Equal(t, testBaseURL+"/auth/login", body.AuthLoginURL)
	assert.Contains(t, body.Message, "Token JWT inválido o firma incorrecta")
}

// TestWriteError_Expired verifies the shape for auth/expired.
func TestWriteError_Expired(t *testing.T) {
	w, body := captureWriteError(CodeExpired, testBaseURL)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, CodeExpired, body.Code)
	assert.Equal(t, "auth", body.Layer)
	assert.Equal(t, testBaseURL+"/auth/login", body.AuthLoginURL)
	assert.Contains(t, body.Message, "Tu sesión expiró")
}

// TestWriteError_Revoked verifies the shape for auth/revoked.
// auth/revoked must NOT include auth_login_url per §F — contact admin instead.
//
// AC-9: auth/revoked must not include auth_login_url.
func TestWriteError_Revoked(t *testing.T) {
	w, body := captureWriteError(CodeRevoked, testBaseURL)

	assert.Equal(t, http.StatusForbidden, w.Code, "auth/revoked must return 403")
	assert.Equal(t, CodeRevoked, body.Code)
	assert.Equal(t, "auth", body.Layer)
	assert.Empty(t, body.AuthLoginURL,
		"auth/revoked must NOT include auth_login_url per §F")
	assert.Contains(t, body.Message, "Tu acceso fue revocado por el administrador",
		"message must contain the Spanish revocation text")
}

// TestWriteError_WebhookInvalidSignature verifies that auth/webhook-invalid-signature
// does NOT include auth_login_url — it's an internal/operator error, not a user error.
//
// AC-9: auth/webhook-invalid-signature must not include auth_login_url.
func TestWriteError_WebhookInvalidSignature(t *testing.T) {
	w, body := captureWriteError(CodeWebhookInvalidSignature, testBaseURL)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, CodeWebhookInvalidSignature, body.Code)
	assert.Equal(t, "auth", body.Layer)
	assert.Empty(t, body.AuthLoginURL,
		"auth/webhook-invalid-signature must NOT include auth_login_url")
	assert.Contains(t, body.Message, "HMAC del webhook no coincide")
}

// TestWriteError_AllCodesHaveKnownStatus verifies that every code in the closed
// list maps to a known HTTP status (no fallback to 500 for known codes).
func TestWriteError_AllCodesHaveKnownStatus(t *testing.T) {
	cases := []struct {
		code           string
		wantStatus     int
		wantLoginURL   bool
	}{
		{CodeUnauthenticated, http.StatusUnauthorized, true},
		{CodeInvalidToken, http.StatusUnauthorized, true},
		{CodeExpired, http.StatusUnauthorized, true},
		{CodeRevoked, http.StatusForbidden, false},
		{CodeInvalidSupabaseToken, http.StatusUnauthorized, true},
		{CodeEmailNotConfirmed, http.StatusForbidden, true},
		{CodeExchangeMalformed, http.StatusBadRequest, false},
		{CodeJWTIssuanceFailed, http.StatusInternalServerError, false},
		{CodeWebhookInvalidSignature, http.StatusUnauthorized, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.code, func(t *testing.T) {
			w, body := captureWriteError(tc.code, testBaseURL)

			assert.Equal(t, tc.wantStatus, w.Code,
				"%s: unexpected HTTP status", tc.code)
			assert.Equal(t, tc.code, body.Code,
				"%s: code field must echo the input code", tc.code)
			assert.Equal(t, "auth", body.Layer,
				"%s: layer must always be 'auth'", tc.code)
			assert.NotEmpty(t, body.Message,
				"%s: message must not be empty", tc.code)

			if tc.wantLoginURL {
				assert.Equal(t, testBaseURL+"/auth/login", body.AuthLoginURL,
					"%s: auth_login_url must be set", tc.code)
			} else {
				assert.Empty(t, body.AuthLoginURL,
					"%s: auth_login_url must be absent", tc.code)
			}
		})
	}
}

// TestWriteError_EmptyBaseURL verifies that when baseURL is empty, auth_login_url
// is omitted even for codes that normally include it.
func TestWriteError_EmptyBaseURL(t *testing.T) {
	w, body := captureWriteError(CodeUnauthenticated, "")

	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Empty(t, body.AuthLoginURL,
		"auth_login_url must be absent when baseURL is empty")
	require.NotEmpty(t, body.Message,
		"message must still be present even without baseURL")
}

// TestWriteError_JSONValid verifies that the response body is always valid JSON.
func TestWriteError_JSONValid(t *testing.T) {
	codes := []string{
		CodeUnauthenticated, CodeInvalidToken, CodeExpired, CodeRevoked,
		CodeWebhookInvalidSignature,
	}

	for _, code := range codes {
		code := code
		t.Run(code, func(t *testing.T) {
			w := httptest.NewRecorder()
			WriteError(w, code, testBaseURL)

			var raw map[string]any
			err := json.Unmarshal(w.Body.Bytes(), &raw)
			require.NoError(t, err, "%s: response body must be valid JSON", code)
		})
	}
}
