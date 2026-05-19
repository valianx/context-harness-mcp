package ratelimit

import "context"

// subKey is the context key type for the authenticated user's sub claim.
// Using an unexported type prevents collisions with keys from other packages.
type subKey struct{}

// ContextWithSub returns a derived context carrying the JWT sub claim.
// Called by the auth middleware after successful token validation so the
// rate limiter can key buckets by user identity rather than IP.
func ContextWithSub(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, subKey{}, sub)
}

// SubFromContext extracts the sub claim stored by ContextWithSub.
// Returns an empty string when no sub is present (MCP_AUTH=none or stdio).
func SubFromContext(ctx context.Context) string {
	v, _ := ctx.Value(subKey{}).(string)
	return v
}
