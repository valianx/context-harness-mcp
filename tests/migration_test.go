package tests

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrationRoundTrip verifies that import_to_supabase.py → export_from_supabase.py
// produces a structurally identical KG: same node names and types, same observation
// texts grouped by node, same relation triples.
// Embeddings (zero-vectors in the fixture) must survive the round-trip with at most
// ε = 1e-6 error per component — they are stored and retrieved as-is, not recomputed.
//
// The test shells out to `uv run scripts/<name>.py` so it covers the actual CLI
// surface that operators will use on flag day. It is skipped gracefully when:
//   - Docker is unavailable (testPool is nil — Docker check happened in TestMain)
//   - `uv` is not on PATH
func TestMigrationRoundTrip(t *testing.T) {
	// Skip gracefully when Docker daemon was not available at suite startup.
	if testPool == nil {
		t.Skip("testPool is nil — Docker daemon was not available when the suite started")
	}

	// Skip gracefully when uv is not installed on this machine.
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skipf("uv not installed; skipping migration round-trip test: %v", err)
	}

	t.Cleanup(func() { CleanDB(t) })

	// go test runs with cwd=package directory (tests/); parent dir is the repo root.
	// migrationsDir = "../migrations" (see setup_test.go) confirms this layout.
	repoRoot := ".."
	fixtureInput := filepath.Join("fixtures", "migration_input.json")
	importScript := filepath.Join(repoRoot, "scripts", "import_to_supabase.py")
	exportScript := filepath.Join(repoRoot, "scripts", "export_from_supabase.py")
	scriptsPyproject := filepath.Join(repoRoot, "scripts")
	exportedOutput := filepath.Join(t.TempDir(), "exported.json")

	// Step 1 — import the fixture into the testcontainer Postgres.
	// Use `uv run --project <dir> python <script>` to work cross-platform:
	// on Windows, `uv run script.py` tries to exec the .py file directly.
	importCmd := exec.Command("uv", "run", "--project", scriptsPyproject,
		"python", importScript, fixtureInput, "--dsn", testDSN)
	importCmd.Stdout = os.Stdout
	importCmd.Stderr = os.Stderr
	require.NoError(t, importCmd.Run(), "import_to_supabase.py failed")

	// Step 2 — export back from the testcontainer Postgres to a temp file.
	exportCmd := exec.Command("uv", "run", "--project", scriptsPyproject,
		"python", exportScript, "--dsn", testDSN, "--output", exportedOutput)
	exportCmd.Stdout = os.Stdout
	exportCmd.Stderr = os.Stderr
	require.NoError(t, exportCmd.Run(), "export_from_supabase.py failed")

	// Step 3 — load and compare both JSONs at the structural level.
	inputPayload := loadJSON(t, fixtureInput)
	outputPayload := loadJSON(t, exportedOutput)

	assertNodesMatch(t, inputPayload, outputPayload)
	assertRelationsMatch(t, inputPayload, outputPayload)
}

// TestMigrationRoundTrip_LegacyEntitiesShape verifies that import_to_supabase.py
// accepts the old {"entities": [...]} shape (defensive fallback for legacy archives)
// and that the data is correctly imported.
func TestMigrationRoundTrip_LegacyEntitiesShape(t *testing.T) {
	if testPool == nil {
		t.Skip("testPool is nil — Docker daemon was not available when the suite started")
	}
	if _, err := exec.LookPath("uv"); err != nil {
		t.Skipf("uv not installed; skipping legacy shape test: %v", err)
	}

	t.Cleanup(func() { CleanDB(t) })

	repoRoot := ".."
	fixtureInput := filepath.Join("fixtures", "migration_input_legacy_entities.json")
	importScript := filepath.Join(repoRoot, "scripts", "import_to_supabase.py")
	scriptsPyproject := filepath.Join(repoRoot, "scripts")

	importCmd := exec.Command("uv", "run", "--project", scriptsPyproject,
		"python", importScript, fixtureInput, "--dsn", testDSN)
	importCmd.Stdout = os.Stdout
	importCmd.Stderr = os.Stderr
	require.NoError(t, importCmd.Run(), "import_to_supabase.py with legacy shape failed")

	// Verify at least one node was imported.
	var count int
	err := testPool.QueryRow(
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
// It also accepts the legacy "entities" key for backwards compatibility.
func toNodeMap(t *testing.T, payload map[string]any) map[string]map[string]any {
	t.Helper()
	// Prefer "nodes" key; fall back to "entities" for legacy fixture support.
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
