package validate_test

// projectid_test.go: unit tests for the projectid.go Check function and
// DefaultProjectID constant introduced in PR-1 (v0.4.0).
//
// AC-4: VERIFY Check accepts valid identifiers and rejects invalid ones with
//       Code=CodeProjectNamingViolation and Layer=LayerProject.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// ── AC-4: TestValidateProjectID_AcceptsValid ──────────────────────────────────

// TestValidateProjectID_AcceptsValid asserts that Check returns nil for all
// identifiers that comply with ^[a-z]([a-z0-9-]{0,62}[a-z0-9])?$.
func TestValidateProjectID_AcceptsValid(t *testing.T) {
	// Build the 64-character boundary valid name: starts with 'a', followed by
	// 62 lowercase letters, ends with 'z' — total 64 chars, no trailing dash.
	maxValid := "a" + strings.Repeat("b", 62) + "z" // 1 + 62 + 1 = 64 chars

	cases := []struct {
		name  string
		input string
	}{
		{name: "global", input: "global"},
		{name: "foo", input: "foo"},
		{name: "foo-bar", input: "foo-bar"},
		{name: "single_letter_a", input: "a"},
		{name: "team-a-prod", input: "team-a-prod"},
		{name: "abc123", input: "abc123"},
		{name: "max_length_64_chars", input: maxValid},
		{name: "starts_with_letter_ends_with_digit", input: "abc-123"},
		{name: "zippy-backoffice", input: "zippy-backoffice"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := validate.Check(tc.input)
			assert.Nil(t, got,
				"Check(%q) debe devolver nil para identificador válido", tc.input)
		})
	}
}

// ── AC-4: TestValidateProjectID_RejectsInvalid ────────────────────────────────

// TestValidateProjectID_RejectsInvalid asserts that Check returns a *Error with
// Code=CodeProjectNamingViolation and Layer=LayerProject for all invalid names.
func TestValidateProjectID_RejectsInvalid(t *testing.T) {
	// 65-char name: starts with 'a', 63 'b's, ends with 'z' — one over the cap.
	overCap := "a" + strings.Repeat("b", 63) + "z" // 1 + 63 + 1 = 65 chars

	cases := []struct {
		name  string
		input string
		desc  string
	}{
		{name: "empty_string", input: "", desc: "string vacío"},
		{name: "uppercase_Foo", input: "Foo", desc: "letra mayúscula"},
		{name: "leading_dash", input: "-foo", desc: "guión al inicio"},
		{name: "trailing_dash", input: "foo-", desc: "guión al final (AC-4 explícito)"},
		{name: "underscore", input: "foo_bar", desc: "guión bajo (underscore no permitido)"},
		{name: "dot", input: "foo.bar", desc: "punto no permitido"},
		{name: "leading_digit", input: "123foo", desc: "empieza con dígito"},
		{name: "over_64_chars", input: overCap, desc: "65 caracteres (sobre el máximo de 64)"},
		{name: "slash", input: "foo/bar", desc: "barra no permitida"},
		{name: "mixed_case", input: "FooBar", desc: "mezcla de mayúsculas"},
		{name: "space", input: "foo bar", desc: "espacio no permitido"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := validate.Check(tc.input)
			require.NotNil(t, got,
				"Check(%q) debe devolver un *Error para: %s", tc.input, tc.desc)
			assert.Equal(t, validate.CodeProjectNamingViolation, got.Code,
				"Check(%q): Code debe ser CodeProjectNamingViolation", tc.input)
			assert.Equal(t, validate.LayerProject, got.Layer,
				"Check(%q): Layer debe ser LayerProject", tc.input)
			assert.NotEmpty(t, got.Message,
				"Check(%q): Message no debe estar vacío", tc.input)
		})
	}
}

// ── AC-5 (partial): TestDefaultProjectID ─────────────────────────────────────

// TestDefaultProjectID asserts that the exported DefaultProjectID constant is
// exactly "global". This constant is the implicit project for all nodes and
// relations created before v0.4.0 and for any write call omitting the project
// field.
//
// AC-5: VERIFY DefaultProjectID == "global".
func TestDefaultProjectID(t *testing.T) {
	assert.Equal(t, "global", validate.DefaultProjectID,
		"DefaultProjectID debe ser la string exacta 'global'")
}
