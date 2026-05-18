package ratelimit

import "context"

// IPKey is the context key type used to propagate the client IP from the HTTP
// layer into MCP tool handlers. Using an unexported type prevents key collisions
// with other packages. The zero value (empty string) means "no IP available"
// (stdio transport); rate-limiting is skipped in that case.
type IPKey struct{}

// ContextWithIP returns a derived context carrying ip under IPKey{}.
// Called by the HTTP context function wired into NewStreamableHTTPServer.
func ContextWithIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, IPKey{}, ip)
}

// IPFromContext extracts the client IP stored by ContextWithIP. Returns an
// empty string when no IP is present (e.g. stdio transport).
func IPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(IPKey{}).(string)
	return ip
}
