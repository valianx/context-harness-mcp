// Package store — collapsed_sql_test.go
//
// Unit tests for CollapsedSQL (AC-S.2). These tests do NOT require Docker —
// they exercise only the whitespace-collapse helper used as the otelpgx
// WithSpanNameFunc to produce readable single-line SQL span names.
package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCollapsedSQL_AC_S2 verifies that CollapsedSQL collapses all whitespace
// runs to a single space and trims leading/trailing whitespace (AC-S.2).
// This is the same function wired as otelpgx.WithSpanNameFunc in store.New so
// that span names in Axiom appear as "query SELECT id FROM nodes WHERE ..."
// instead of the raw multi-line Go string.
func TestCollapsedSQL_AC_S2(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name: "multi_line_select",
			input: `SELECT n.id
		FROM nodes n
		WHERE n.name = $1`,
			want: "SELECT n.id FROM nodes n WHERE n.name = $1",
		},
		{
			name:  "single_line_unchanged",
			input: "SELECT id FROM nodes WHERE name = $1",
			want:  "SELECT id FROM nodes WHERE name = $1",
		},
		{
			name:  "leading_trailing_whitespace_trimmed",
			input: "\n\t  SELECT 1  \n",
			want:  "SELECT 1",
		},
		{
			name:  "tabs_and_mixed_whitespace",
			input: "INSERT\t\tINTO nodes\n\t(name)\n\tVALUES ($1)",
			want:  "INSERT INTO nodes (name) VALUES ($1)",
		},
		{
			name:  "empty_string",
			input: "",
			want:  "",
		},
		{
			name:  "only_whitespace_collapses_to_empty",
			input: "\n\t  \n",
			want:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CollapsedSQL(tc.input)
			assert.Equal(t, tc.want, got,
				"CollapsedSQL(%q) = %q, want %q", tc.input, got, tc.want)
		})
	}
}
