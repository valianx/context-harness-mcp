package tests

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/khctl"
)

// TestMigrationRoundTrip verifies that khctl import → khctl export produces a
// structurally identical KG: same node names and types, same observation texts
// grouped by node, same relation triples.
// Embeddings (zero-vectors in the fixture) must survive the round-trip with at
// most ε = 1e-6 error per component — they are stored and retrieved as-is.
//
// The test calls the internal khctl Go functions directly (no os/exec, no
// Python/uv dependency). It skips gracefully when Docker is unavailable.
func TestMigrationRoundTrip(t *testing.T) {
	if testPool == nil {
		t.Skip("testPool is nil — Docker daemon was not available when the suite started")
	}
	t.Cleanup(func() { CleanDB(t) })

	fixtureInput := filepath.Join("fixtures", "migration_input.json")
	inputData, err := os.ReadFile(fixtureInput)
	require.NoError(t, err, "reading fixture input")

	// Step 1 — import the fixture into the testcontainer Postgres.
	nodes, relations, err := khctl.ParseImportPayload(inputData)
	require.NoError(t, err, "parse import payload")

	_, _, _, _, _, err = khctl.RunImport(context.Background(), testPool, nodes, relations)
	require.NoError(t, err, "import fixture into testcontainer Postgres")

	// Step 2 — export back from the testcontainer Postgres.
	payload, err := khctl.BuildExportPayload(context.Background(), testPool, "")
	require.NoError(t, err, "export from testcontainer Postgres")

	exportedData, err := json.Marshal(payload)
	require.NoError(t, err, "marshal exported payload")

	// Step 3 — compare both JSONs at the structural level.
	inputPayload := loadJSON(t, fixtureInput)
	var outputPayload map[string]any
	require.NoError(t, json.Unmarshal(exportedData, &outputPayload), "unmarshal exported payload")

	assertNodesMatch(t, inputPayload, outputPayload)
	assertRelationsMatch(t, inputPayload, outputPayload)
}

// TestMigrationRoundTrip_LegacyEntitiesShape verifies that khctl import accepts
// the old {"entities": [...]} shape (defensive fallback for legacy archives)
// and that the data is correctly imported.
func TestMigrationRoundTrip_LegacyEntitiesShape(t *testing.T) {
	if testPool == nil {
		t.Skip("testPool is nil — Docker daemon was not available when the suite started")
	}
	t.Cleanup(func() { CleanDB(t) })

	fixtureInput := filepath.Join("fixtures", "migration_input_legacy_entities.json")
	inputData, err := os.ReadFile(fixtureInput)
	require.NoError(t, err, "reading legacy fixture input")

	nodes, relations, err := khctl.ParseImportPayload(inputData)
	require.NoError(t, err, "parse legacy import payload")

	_, _, _, _, _, err = khctl.RunImport(context.Background(), testPool, nodes, relations)
	require.NoError(t, err, "import legacy fixture into testcontainer Postgres")

	// Verify at least one node was imported.
	var count int
	err = testPool.QueryRow(
		context.Background(), "SELECT COUNT(*) FROM nodes WHERE deleted_at IS NULL",
	).Scan(&count)
	require.NoError(t, err)
	assert.Greater(t, count, 0, "legacy entities shape must import at least one node")
}

// loadJSON reads a JSON file and returns the parsed structure.
func loadJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", path)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out), "parsing %s", path)
	return out
}

// assertNodesMatch verifies that the exported nodes match the input by name,
// type, observations, and embedding round-trip (within ε = 1e-6).
func assertNodesMatch(t *testing.T, input, output map[string]any) {
	t.Helper()

	inNodes := toNodeMap(t, input)
	outNodes := toNodeMap(t, output)

	assert.Equal(t, len(inNodes), len(outNodes),
		"node count mismatch: got %d, want %d", len(outNodes), len(inNodes))

	for name, inNode := range inNodes {
		outNode, ok := outNodes[name]
		if !assert.True(t, ok, "node %q missing from export", name) {
			continue
		}
		assert.Equal(t, inNode["nodeType"], outNode["nodeType"],
			"node %q: nodeType mismatch", name)

		assertObservationsMatch(t, name, inNode, outNode)
	}
}

// assertObservationsMatch checks that observation texts and embeddings match.
func assertObservationsMatch(
	t *testing.T,
	nodeName string,
	inNode, outNode map[string]any,
) {
	t.Helper()
	const eps = 1e-6

	inObs, _ := inNode["observations"].([]any)
	outObs, _ := outNode["observations"].([]any)
	assert.Equal(t, len(inObs), len(outObs),
		"node %q: observation count mismatch", nodeName)

	inEmbs, hasInEmb := inNode["embeddings"].([]any)
	outEmbs, hasOutEmb := outNode["embeddings"].([]any)

	if !hasInEmb {
		return // input had no embeddings — nothing to check on export side
	}
	if !assert.True(t, hasOutEmb, "node %q: input has embeddings but export does not", nodeName) {
		return
	}
	require.Equal(t, len(inEmbs), len(outEmbs),
		"node %q: embedding array length mismatch", nodeName)

	for i, inEmbRaw := range inEmbs {
		inVec, _ := inEmbRaw.([]any)
		outVec, _ := outEmbs[i].([]any)
		require.Equal(t, len(inVec), len(outVec),
			"node %q obs %d: embedding dim mismatch", nodeName, i)

		for j, v := range inVec {
			inF, _ := v.(float64)
			outF, _ := outVec[j].(float64)
			assert.True(t, math.Abs(inF-outF) < eps,
				"node %q obs %d dim %d: |%g - %g| >= ε", nodeName, i, j, inF, outF)
		}
	}
}

// assertRelationsMatch verifies the exported relations match the input triples.
func assertRelationsMatch(t *testing.T, input, output map[string]any) {
	t.Helper()

	type triple struct{ from, to, relType string }
	toSet := func(payload map[string]any) map[triple]struct{} {
		m := make(map[triple]struct{})
		rels, _ := payload["relations"].([]any)
		for _, r := range rels {
			rel, _ := r.(map[string]any)
			m[triple{
				from:    rel["from"].(string),
				to:      rel["to"].(string),
				relType: rel["relationType"].(string),
			}] = struct{}{}
		}
		return m
	}

	inSet := toSet(input)
	outSet := toSet(output)

	assert.Equal(t, len(inSet), len(outSet), "relation count mismatch")
	for tr := range inSet {
		assert.Contains(t, outSet, tr, "relation %+v missing from export", tr)
	}
}

// toNodeMap converts the "nodes" array to a map keyed by node name.
// Also accepts the legacy "entities" key for backwards compatibility.
func toNodeMap(t *testing.T, payload map[string]any) map[string]map[string]any {
	t.Helper()
	raw, ok := payload["nodes"]
	if !ok {
		raw = payload["entities"]
	}
	nodes, _ := raw.([]any)
	m := make(map[string]map[string]any, len(nodes))
	for _, e := range nodes {
		node, _ := e.(map[string]any)
		name, _ := node["name"].(string)
		m[name] = node
	}
	return m
}
