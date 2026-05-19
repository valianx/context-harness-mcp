// Package auth implements JWT-based authentication for the MCP server.
// It provides token issuance, validation, HTTP middleware, revocation cache,
// and structured error helpers.
package auth

import "context"

// userIDKey is the context key type for the authenticated user's Supabase UUID.
// Using an unexported type prevents collisions with keys from other packages.
type userIDKey struct{}

// emailKey is the context key type for the authenticated user's email.
type emailKey struct{}

// WithUserID returns a derived context carrying the given Supabase user UUID.
// Called by the auth middleware on successful token validation.
func WithUserID(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, userIDKey{}, sub)
}

// WithEmail returns a derived context carrying the given email address.
// Called by the auth middleware on successful token validation.
func WithEmail(ctx context.Context, email string) context.Context {
	return context.WithValue(ctx, emailKey{}, email)
}

// UserIDFromContext extracts the Supabase user UUID stored by WithUserID.
// Returns an empty string when no user is authenticated (MCP_AUTH=none or stdio).
func UserIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(userIDKey{}).(string)
	return v
}

// EmailFromContext extracts the user email stored by WithEmail.
// Returns an empty string when no user is authenticated.
func EmailFromContext(ctx context.Context) string {
	v, _ := ctx.Value(emailKey{}).(string)
	return v
}
