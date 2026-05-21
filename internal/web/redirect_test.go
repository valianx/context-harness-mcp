package web

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsSafe exercises the complete IsSafe allowlist with table-driven cases.
// Every rejection case from 01-architecture.md §Viewer auth-gating is covered.
func TestIsSafe(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		// ── accepted ──────────────────────────────────────────────────────────
		{name: "dashboard_exact", input: "/dashboard", want: true},
		{name: "viewer_exact", input: "/viewer/", want: true},
		{name: "viewer_deep_link", input: "/viewer/anything", want: true},
		{name: "viewer_query_string", input: "/viewer/?q=foo", want: true},
		{name: "viewer_deep_with_query", input: "/viewer/subpath?q=bar&x=1", want: true},
		{name: "viewer_api_path", input: "/viewer/api/search", want: true},

		// ── rejected: empty/blank ──────────────────────────────────────────────
		{name: "empty_string", input: "", want: false},

		// ── rejected: scheme-present ──────────────────────────────────────────
		{name: "https_absolute", input: "https://evil.com/phish", want: false},
		{name: "http_absolute", input: "http://evil.com/phish", want: false},
		{name: "javascript_xss", input: "javascript:alert(1)", want: false},
		{name: "mailto", input: "mailto:foo@bar.com", want: false},
		{name: "ftp", input: "ftp://files.evil.com", want: false},
		{name: "data_uri", input: "data:text/html,<script>alert(1)</script>", want: false},

		// ── rejected: protocol-relative ───────────────────────────────────────
		{name: "protocol_relative", input: "//evil.com/phish", want: false},
		{name: "protocol_relative_slash", input: "//evil.com", want: false},

		// ── rejected: Windows-style slash normalization ────────────────────────
		{name: "windows_slash", input: `/\evil`, want: false},

		// ── rejected: paths outside allowlist ─────────────────────────────────
		{name: "root", input: "/", want: false},
		{name: "admin", input: "/admin", want: false},
		{name: "mcp", input: "/mcp/", want: false},
		{name: "auth_login", input: "/auth/login", want: false},
		{name: "auth_exchange", input: "/auth/exchange", want: false},
		{name: "healthz", input: "/healthz", want: false},
		{name: "viewer_without_trailing_slash", input: "/viewer", want: false},
		{name: "dashboard_subpath", input: "/dashboard/generate-token", want: false},
		{name: "dashboard_extra", input: "/dashboards", want: false},

		// ── rejected: path traversal ───────────────────────────────────────────
		// path.Clean resolves ".." segments before allowlist matching:
		//   "/viewer/../../etc/passwd" → "/etc/passwd"    (not in allowlist)
		//   "/dashboard/../etc/passwd" → "/etc/passwd"    (not in allowlist)
		{name: "traversal_viewer", input: "/viewer/../../etc/passwd", want: false},
		{name: "traversal_dashboard", input: "/dashboard/../etc/passwd", want: false},

		// ── rejected: bare hostname ───────────────────────────────────────────
		{name: "no_leading_slash", input: "evil.com", want: false},
		{name: "no_leading_slash_path", input: "evil.com/path", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsSafe(tc.input)
			assert.Equal(t, tc.want, got,
				"IsSafe(%q) = %v, want %v", tc.input, got, tc.want)
		})
	}
}
