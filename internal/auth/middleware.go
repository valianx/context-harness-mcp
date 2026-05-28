package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// RevocationStore is the minimal DB interface the middleware needs.
// Using a small interface (not *pgxpool.Pool) lets tests inject a fake.
type RevocationStore interface {
	// GetRevoked returns true when the user identified by sub has a non-null
	// revoked_at in the users table. Returns an error on DB failure.
	GetRevoked(sub string) (bool, error)
}

// Middleware wraps handler with JWT bearer authentication when mode is ModeEnabled.
// When mode is ModeNone the original handler is returned unchanged, preserving
// full back-compat with pre-Phase-0 consumers (no 401, no overhead).
//
// On a valid token the middleware injects sub and email into the request context
// via WithUserID / WithEmail so downstream handlers can read attribution.
//
// Ordering guarantee: auth runs before the MCP handler, which runs Content Filter
// before any DB write. A 401/403 from this middleware aborts before payload
// validation (satisfies AC-12).
func Middleware(mode Mode, store RevocationStore, cache *RevocationCache, baseURL string, handler http.Handler) http.Handler {
	if mode == ModeNone {
		// Pass-through: zero overhead for the back-compat window.
		return handler
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := extractBearer(r)
		if !ok {
			WriteError(w, CodeUnauthenticated, resolveBaseURL(baseURL, r))
			return
		}

		claims, err := ValidateMCPToken(token)
		if err != nil {
			code := classifyTokenError(err)
			WriteError(w, code, resolveBaseURL(baseURL, r))
			return
		}

		revoked, cacheHit, err := checkRevocation(claims.Subject, store, cache)
		if err != nil {
			slog.Error("revocation check failed", "sub_prefix", subPrefix(claims.Subject), "error", err)
			// Fail open on DB error — do not deny the user on a transient DB outage.
			// The revocation cache means we had a recent successful check.
		}
		if revoked {
			WriteError(w, CodeRevoked, resolveBaseURL(baseURL, r))
			return
		}

		// Set span attributes on the active (otelhttp-created) span so the HTTP
		// parent span carries user identity and cache performance data (AC-C.3, AC-C.4).
		span := trace.SpanFromContext(r.Context())
		span.SetAttributes(
			attribute.String("user.id", claims.Subject),
			attribute.String("auth.cache_result", cacheResult(cacheHit)),
		)

		ctx := WithUserID(r.Context(), claims.Subject)
		ctx = WithEmail(ctx, claims.Email)

		handler.ServeHTTP(w, r.WithContext(ctx))
	})
}

// extractBearer parses the Authorization header for a Bearer token.
// Returns (token, true) on success or ("", false) when the header is absent
// or does not start with exactly "Bearer ".
func extractBearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	if !strings.HasPrefix(h, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(h, "Bearer ")
	if token == "" {
		return "", false
	}
	return token, true
}

// classifyTokenError maps a jwt parse/validation error to an auth error code.
// Expired tokens get CodeExpired; everything else maps to CodeInvalidToken.
func classifyTokenError(err error) string {
	if errors.Is(err, jwt.ErrTokenExpired) {
		return CodeExpired
	}
	return CodeInvalidToken
}

// checkRevocation checks the revocation cache and falls back to the DB.
// Returns (revoked, cacheHit, nil) when the check succeeds, or (false, false, err)
// when the DB query failed (fail-open on DB error).
// cacheHit=true means the result came from the LRU cache (no DB round-trip).
func checkRevocation(sub string, store RevocationStore, cache *RevocationCache) (revoked bool, cacheHit bool, err error) {
	if revoked, ok := cache.Get(sub); ok {
		return revoked, true, nil
	}

	revoked, err = store.GetRevoked(sub)
	if err != nil {
		return false, false, err
	}

	cache.Set(sub, revoked)
	return revoked, false, nil
}

// cacheResult converts the cacheHit boolean to the canonical span attribute value.
func cacheResult(hit bool) string {
	if hit {
		return "hit"
	}
	return "miss"
}

// resolveBaseURL returns baseURL when non-empty, otherwise derives it from the
// request Host header (scheme defaults to https for non-localhost hosts).
func resolveBaseURL(baseURL string, r *http.Request) string {
	if baseURL != "" {
		return baseURL
	}
	host := r.Host
	if host == "" {
		return ""
	}
	scheme := "https"
	if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") {
		scheme = "http"
	}
	return scheme + "://" + host
}

// subPrefix returns the first 8 characters of a UUID for safe logging.
// Never log the full token or full sub — partial UUID is sufficient for
// correlation without exposing the full identifier.
func subPrefix(sub string) string {
	if len(sub) <= 8 {
		return sub
	}
	return sub[:8]
}

// ClassifyJWTError is exported for use in tests that want to assert error codes
// without importing the middleware package from outside internal/.
func ClassifyJWTError(err error) string {
	return classifyTokenError(err)
}
