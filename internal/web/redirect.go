package web

import (
	"net/url"
	"path"
	"strings"
)

// IsSafe returns true when next is a safe same-origin redirect target.
//
// Accepted paths (the complete allowlist):
//   - "/dashboard" (exact)
//   - "/viewer/" (exact)
//   - anything that starts with "/viewer/" (deep links, query strings OK)
//
// Rejected inputs include:
//   - empty string
//   - absolute URLs with a scheme (https://, http://, javascript:, etc.)
//   - protocol-relative URLs ("//evil.com")
//   - Windows-style slash normalization ("/\evil")
//   - path traversal that escapes the allowlist ("/viewer/../../etc/passwd")
//   - any other path not in the allowlist ("/admin", "/mcp/", etc.)
//
// The function is the single source of truth for the open-redirect allowlist.
// It is invoked at two server-side trust boundaries: GET /auth/login (template
// render) and POST /auth/exchange (response redirect_to field). Never trust the
// client to have validated — this runs on every inbound value.
func IsSafe(next string) bool {
	if next == "" {
		return false
	}

	// Must start with "/" — no absolute URLs or bare hostnames.
	if !strings.HasPrefix(next, "/") {
		return false
	}

	// Reject protocol-relative ("//evil.com") and Windows slash ("/\evil").
	if strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return false
	}

	// Parse to validate: scheme and host must both be empty (same-origin).
	parsed, err := url.Parse(next)
	if err != nil {
		return false
	}
	if parsed.Scheme != "" || parsed.Host != "" {
		return false
	}

	// Clean the path to eliminate ".." traversal and double-slashes before
	// allowlist matching. path.Clean resolves ".." segments, making
	// "/viewer/../../etc/passwd" → "/etc/passwd" which then fails the check.
	cleanPath := path.Clean(parsed.Path)
	if cleanPath == "" || cleanPath == "." {
		cleanPath = "/"
	}

	// Re-attach a trailing slash when the original parsed path had one and
	// cleaning removed it — this preserves "/viewer/" as distinct from "/viewer".
	// path.Clean removes trailing slashes, so "/viewer/" becomes "/viewer".
	// We want "/viewer/" to be accepted (it's in the allowlist) but "/viewer"
	// (without trailing slash) is NOT accepted (it bypasses the trailing-slash
	// requirement that prevents matching "/viewerXXX" sub-paths).
	if strings.HasSuffix(parsed.Path, "/") && !strings.HasSuffix(cleanPath, "/") {
		cleanPath += "/"
	}

	return isAllowedPath(cleanPath)
}

// isAllowedPath checks whether cleanPath falls under the allowlist.
// The path has already been cleaned with path.Clean so traversal is resolved.
func isAllowedPath(cleanPath string) bool {
	if cleanPath == "/dashboard" {
		return true
	}
	// "/viewer/" exact OR any deep link under "/viewer/".
	// The trailing slash in "/viewer/" is required — it prevents a future
	// "/viewerXXX" path from accidentally matching.
	if cleanPath == "/viewer/" || strings.HasPrefix(cleanPath, "/viewer/") {
		return true
	}
	return false
}
