package validate_test

// errors_test.go: unit tests for the policy error code constants introduced in
// PR-1 (v0.4.0).
//
// AC-5: VERIFY CodeProjectNamingViolation, CodeCrossProjectRelation,
//       CodeNodeNotFound, CodeRelationAlreadyExists are non-empty strings
//       starting with "policy/" and LayerProject == "project".

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// ── AC-5: TestNewErrorCodes_Defined ──────────────────────────────────────────

// TestNewErrorCodes_Defined asserts that the four new policy error codes are
// non-empty strings beginning with "policy/" and that LayerProject == "project".
func TestNewErrorCodes_Defined(t *testing.T) {
	type codeCase struct {
		name  string
		value string
		exact string // expected exact value from AC-5 spec
	}

	cases := []codeCase{
		{
			name:  "CodeProjectNamingViolation",
			value: validate.CodeProjectNamingViolation,
			exact: "policy/project-naming-violation",
		},
		{
			name:  "CodeCrossProjectRelation",
			value: validate.CodeCrossProjectRelation,
			exact: "policy/cross-project-relation",
		},
		{
			name:  "CodeNodeNotFound",
			value: validate.CodeNodeNotFound,
			exact: "policy/node-not-found",
		},
		{
			name:  "CodeRelationAlreadyExists",
			value: validate.CodeRelationAlreadyExists,
			exact: "policy/relation-already-exists",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			assert.NotEmpty(t, tc.value,
				"%s no debe ser una string vacía", tc.name)
			assert.True(t, strings.HasPrefix(tc.value, "policy/"),
				"%s debe comenzar con 'policy/', obtenido: %q", tc.name, tc.value)
			assert.Equal(t, tc.exact, tc.value,
				"%s debe tener el valor exacto %q (inmutable wire-stable)", tc.name, tc.exact)
		})
	}
}

// TestLayerProject_Defined asserts that LayerProject equals "project" and is
// non-empty. This constant identifies the project-scoping layer in policy errors.
func TestLayerProject_Defined(t *testing.T) {
	assert.NotEmpty(t, validate.LayerProject,
		"LayerProject no debe ser una string vacía")
	assert.Equal(t, "project", validate.LayerProject,
		"LayerProject debe ser exactamente 'project'")
}

// TestExistingErrorCodes_Unchanged verifies that the four pre-existing error
// codes and their layer names are unmodified after the PR-1 additions (wire
// stability regression check).
func TestExistingErrorCodes_Unchanged(t *testing.T) {
	existing := map[string]string{
		"CodeSizeExceeded":      validate.CodeSizeExceeded,
		"CodeJunkPattern":       validate.CodeJunkPattern,
		"CodeSecretDetected":    validate.CodeSecretDetected,
		"CodeTaxonomyViolation": validate.CodeTaxonomyViolation,
		"CodeRateLimited":       validate.CodeRateLimited,
	}

	expectedValues := map[string]string{
		"CodeSizeExceeded":      "policy/size-exceeded",
		"CodeJunkPattern":       "policy/junk-pattern",
		"CodeSecretDetected":    "policy/secret-detected",
		"CodeTaxonomyViolation": "policy/taxonomy-violation",
		"CodeRateLimited":       "policy/rate-limited",
	}

	for name, got := range existing {
		name, got := name, got
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, expectedValues[name], got,
				"%s no debe cambiar (wire-stable)", name)
		})
	}
}
