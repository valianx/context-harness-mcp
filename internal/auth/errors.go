package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Auth error code constants — the closed list from §F of 01-architecture.md.
// Clients switch on these stable string values for error handling.
const (
	CodeUnauthenticated          = "auth/unauthenticated"
	CodeInvalidToken             = "auth/invalid-token"
	CodeExpired                  = "auth/expired"
	CodeRevoked                  = "auth/revoked"
	CodeInvalidSupabaseToken     = "auth/invalid-supabase-token"
	CodeEmailNotConfirmed        = "auth/email-not-confirmed"
	CodeExchangeMalformed        = "auth/exchange-malformed"
	CodeJWTIssuanceFailed        = "auth/jwt-issuance-failed"
	CodeWebhookInvalidSignature  = "auth/webhook-invalid-signature"
)

// httpStatus maps each auth error code to its HTTP status code.
var httpStatus = map[string]int{
	CodeUnauthenticated:         http.StatusUnauthorized,
	CodeInvalidToken:            http.StatusUnauthorized,
	CodeExpired:                 http.StatusUnauthorized,
	CodeRevoked:                 http.StatusForbidden,
	CodeInvalidSupabaseToken:    http.StatusUnauthorized,
	CodeEmailNotConfirmed:       http.StatusForbidden,
	CodeExchangeMalformed:       http.StatusBadRequest,
	CodeJWTIssuanceFailed:       http.StatusInternalServerError,
	CodeWebhookInvalidSignature: http.StatusUnauthorized,
}

// spanishMessage returns the hardcoded Spanish message for each error code.
// The {auth_login_url} placeholder is replaced with the actual URL in WriteError.
func spanishMessage(code, loginURL string) string {
	switch code {
	case CodeUnauthenticated:
		return fmt.Sprintf("Falta el header Authorization Bearer. Re-autenticate en %s", loginURL)
	case CodeInvalidToken:
		return fmt.Sprintf("Token JWT inválido o firma incorrecta. Re-autenticate en %s", loginURL)
	case CodeExpired:
		return fmt.Sprintf("Tu sesión expiró. Re-autenticate en %s", loginURL)
	case CodeRevoked:
		return "Tu acceso fue revocado por el administrador. Contactá al admin si creés que es un error."
	case CodeInvalidSupabaseToken:
		return fmt.Sprintf("El access token de Supabase no es válido. Reiniciá el flow desde %s", loginURL)
	case CodeEmailNotConfirmed:
		return fmt.Sprintf("Tu email no está confirmado. Revisá tu inbox y luego volvé a %s", loginURL)
	case CodeExchangeMalformed:
		return "Body del exchange request inválido."
	case CodeJWTIssuanceFailed:
		return "Error interno al emitir el token. Reintenta el login."
	case CodeWebhookInvalidSignature:
		return "HMAC del webhook no coincide."
	default:
		return "Error de autenticación."
	}
}

// codesWithLoginURL lists the error codes that include auth_login_url in the response.
// auth/revoked does NOT include the URL per §F — the user must contact admin.
// auth/webhook-invalid-signature is internal only.
var codesWithLoginURL = map[string]bool{
	CodeUnauthenticated:      true,
	CodeInvalidToken:         true,
	CodeExpired:              true,
	CodeInvalidSupabaseToken: true,
	CodeEmailNotConfirmed:    true,
}

// errorBody is the JSON shape for all auth error responses (§F).
type errorBody struct {
	Code         string `json:"code"`
	Message      string `json:"message"`
	AuthLoginURL string `json:"auth_login_url,omitempty"`
	Layer        string `json:"layer"`
}

// WriteError writes a structured auth error response to w.
// baseURL is used to construct auth_login_url (e.g. "https://mcp.example.com").
// When baseURL is empty, auth_login_url is omitted even for codes that usually include it.
// The function always sets Content-Type: application/json and the appropriate HTTP status.
func WriteError(w http.ResponseWriter, code, baseURL string) {
	loginURL := ""
	if baseURL != "" {
		loginURL = baseURL + "/auth/login"
	}

	body := errorBody{
		Code:    code,
		Message: spanishMessage(code, loginURL),
		Layer:   "auth",
	}

	if codesWithLoginURL[code] && loginURL != "" {
		body.AuthLoginURL = loginURL
	}

	status, ok := httpStatus[code]
	if !ok {
		status = http.StatusInternalServerError
	}

	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data) //nolint:errcheck
}
