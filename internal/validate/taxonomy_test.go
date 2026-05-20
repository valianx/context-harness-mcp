package validate_test

// taxonomy_test.go: unit tests for RelationTypes map additions introduced in
// PR-1 (v0.4.0 taxonomy extension).
//
// AC-6: VERIFY RelationTypes contains exactly 7 entries including the two new
//       values 'supersedes' and 'conflicts_with'.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/validate"
)

// ── AC-6: TestRelationTypes_IncludesNewValues ─────────────────────────────────

// TestRelationTypes_IncludesNewValues asserts that RelationTypes includes both
// new v0.4.0 values and has a total length of 7.
func TestRelationTypes_IncludesNewValues(t *testing.T) {
	t.Run("total_length_is_7", func(t *testing.T) {
		assert.Len(t, validate.RelationTypes, 7,
			"RelationTypes debe tener exactamente 7 entradas")
	})

	t.Run("includes_supersedes", func(t *testing.T) {
		assert.True(t, validate.RelationTypes["supersedes"],
			"RelationTypes debe incluir 'supersedes' (nuevo en v0.4.0)")
	})

	t.Run("includes_conflicts_with", func(t *testing.T) {
		assert.True(t, validate.RelationTypes["conflicts_with"],
			"RelationTypes debe incluir 'conflicts_with' (nuevo en v0.4.0)")
	})

	// Verify all 5 legacy values are still present (back-compat).
	legacyValues := []string{
		"relates_to",
		"belongs-to",
		"calls",
		"uses-stack",
		"depends-on",
	}
	for _, v := range legacyValues {
		v := v
		t.Run("legacy_"+v, func(t *testing.T) {
			assert.True(t, validate.RelationTypes[v],
				"RelationTypes debe seguir incluyendo el valor legacy %q", v)
		})
	}
}

// TestRelationTypes_NewValuesPassValidator exercises the full Content Filter
// path with the two new relation types to confirm checkRelationTypes accepts them.
func TestRelationTypes_NewValuesPassValidator(t *testing.T) {
	newTypes := []string{"supersedes", "conflicts_with"}
	for _, rt := range newTypes {
		rt := rt
		t.Run("validate_run_accepts_"+rt, func(t *testing.T) {
			p := validate.Payload{
				Relations: []validate.Relation{
					{From: "node-a", To: "node-b", RelationType: rt},
				},
			}
			got := validate.Run(&p, validate.KindRelations)
			assert.Nil(t, got,
				"validate.Run deve devolver nil para relation_type=%q (nuevo en v0.4.0)", rt)
		})
	}
}

// TestRelationTypes_InvalidValueRejected confirms the validator still rejects
// values outside the closed enum after the v0.4.0 expansion.
func TestRelationTypes_InvalidValueRejected(t *testing.T) {
	p := validate.Payload{
		Relations: []validate.Relation{
			{From: "node-a", To: "node-b", RelationType: "frobnicates"},
		},
	}
	got := validate.Run(&p, validate.KindRelations)
	require.NotNil(t, got, "valor fuera del enum debe ser rechazado")
	assert.Equal(t, validate.CodeTaxonomyViolation, got.Code,
		"el código de error debe ser CodeTaxonomyViolation")
	assert.Equal(t, validate.LayerTaxonomy, got.Layer,
		"la capa debe ser LayerTaxonomy")
}
