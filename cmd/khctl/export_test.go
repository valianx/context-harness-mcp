package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/khctl"
)

// TestExport_EmptyDB verifies that export produces a valid empty payload when
// there are no active rows.
func TestExport_EmptyDB(t *testing.T) {
	if khctlPool == nil {
		t.Skip("Docker daemon not available")
	}
	t.Cleanup(func() { cleanDB(t) })

	payload, err := khctl.BuildExportPayload(context.Background(), khctlPool)
	require.NoError(t, err)
	assert.Equal(t, khctl.ExportFormatVersion, payload.FormatVersion)
	assert.Equal(t, 0, payload.NodeCount)
	assert.Equal(t, 0, payload.RelationCount)
	assert.Empty(t, payload.Nodes)
	assert.Empty(t, payload.Relations)
}

// TestExport_RoundTrip verifies that import → export produces a structurally
// equivalent payload: same node names/types, same observation texts, same
// relation triples.
func TestExport_RoundTrip(t *testing.T) {
	if khctlPool == nil {
		t.Skip("Docker daemon not available")
	}
	t.Cleanup(func() { cleanDB(t) })

	inputJSON := `{
		"nodes": [
			{
				"name": "round-trip-pattern",
				"nodeType": "pattern",
				"observations": ["First observation.", "Second observation."]
			},
			{
				"name": "round-trip-decision",
				"nodeType": "decision",
				"observations": ["A decision observation."]
			}
		],
		"relations": [
			{"from": "round-trip-pattern", "to": "round-trip-decision", "relationType": "relates_to"}
		]
	}`

	nodes, relations, err := khctl.ParseImportPayload([]byte(inputJSON))
	require.NoError(t, err)

	_, _, _, relsIn, _, err := khctl.RunImport(context.Background(), khctlPool, nodes, relations)
	require.NoError(t, err)
	assert.Equal(t, 1, relsIn, "expected 1 relation inserted")

	payload, err := khctl.BuildExportPayload(context.Background(), khctlPool)
	require.NoError(t, err)

	assert.Equal(t, 2, payload.NodeCount)
	assert.Equal(t, 1, payload.RelationCount)

	nodeMap := make(map[string]khctl.ExportNode, len(payload.Nodes))
	for _, n := range payload.Nodes {
		nodeMap[n.Name] = n
	}

	pat, ok := nodeMap["round-trip-pattern"]
	require.True(t, ok, "round-trip-pattern must be in export")
	assert.Equal(t, "pattern", pat.NodeType)
	assert.Equal(t, []string{"First observation.", "Second observation."}, pat.Observations)

	dec, ok := nodeMap["round-trip-decision"]
	require.True(t, ok, "round-trip-decision must be in export")
	assert.Equal(t, "decision", dec.NodeType)

	require.Len(t, payload.Relations, 1)
	rel := payload.Relations[0]
	assert.Equal(t, "round-trip-pattern", rel.From)
	assert.Equal(t, "round-trip-decision", rel.To)
	assert.Equal(t, "relates_to", rel.RelationType)
}

// TestExport_JSONSerializable verifies that the exported payload marshals to
// valid JSON without error.
func TestExport_JSONSerializable(t *testing.T) {
	if khctlPool == nil {
		t.Skip("Docker daemon not available")
	}
	t.Cleanup(func() { cleanDB(t) })

	payload, err := khctl.BuildExportPayload(context.Background(), khctlPool)
	require.NoError(t, err)

	data, err := json.MarshalIndent(payload, "", "  ")
	require.NoError(t, err)
	assert.NotEmpty(t, data)

	var decoded khctl.ExportPayload
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, payload.FormatVersion, decoded.FormatVersion)
}
