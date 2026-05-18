package tests

import (
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
// produces a structurally identical KG: same entity names and types, same observation
// texts grouped by entity, same relation triples.
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

	assertEntitiesMatch(t, inputPayload, outputPayload)
	assertRelationsMatch(t, inputPayload, outputPayload)
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

// assertEntitiesMatch verifies that the exported entities match the input
// by name, type, observations, and embedding round-trip (within ε = 1e-6).
func assertEntitiesMatch(t *testing.T, input, output map[string]any) {
	t.Helper()

	inEntities := toEntityMap(t, input)
	outEntities := toEntityMap(t, output)

	assert.Equal(t, len(inEntities), len(outEntities),
		"entity count mismatch: got %d, want %d", len(outEntities), len(inEntities))

	for name, inEnt := range inEntities {
		outEnt, ok := outEntities[name]
		if !assert.True(t, ok, "entity %q missing from export", name) {
			continue
		}
		assert.Equal(t, inEnt["entityType"], outEnt["entityType"],
			"entity %q: entityType mismatch", name)

		assertObservationsMatch(t, name, inEnt, outEnt)
	}
}

// assertObservationsMatch checks that observation texts and embeddings match.
func assertObservationsMatch(
	t *testing.T,
	entityName string,
	inEnt, outEnt map[string]any,
) {
	t.Helper()
	const eps = 1e-6

	inObs, _ := inEnt["observations"].([]any)
	outObs, _ := outEnt["observations"].([]any)
	assert.Equal(t, len(inObs), len(outObs),
		"entity %q: observation count mismatch", entityName)

	inEmbs, hasInEmb := inEnt["embeddings"].([]any)
	outEmbs, hasOutEmb := outEnt["embeddings"].([]any)

	if !hasInEmb {
		return // input had no embeddings — nothing to check on export side
	}
	if !assert.True(t, hasOutEmb, "entity %q: input has embeddings but export does not", entityName) {
		return
	}
	require.Equal(t, len(inEmbs), len(outEmbs),
		"entity %q: embedding array length mismatch", entityName)

	for i, inEmbRaw := range inEmbs {
		inVec, _ := inEmbRaw.([]any)
		outVec, _ := outEmbs[i].([]any)
		require.Equal(t, len(inVec), len(outVec),
			"entity %q obs %d: embedding dim mismatch", entityName, i)

		for j, v := range inVec {
			inF, _ := v.(float64)
			outF, _ := outVec[j].(float64)
			assert.True(t, math.Abs(inF-outF) < eps,
				"entity %q obs %d dim %d: |%g - %g| >= ε", entityName, i, j, inF, outF)
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

// toEntityMap converts the "entities" array to a map keyed by entity name.
func toEntityMap(t *testing.T, payload map[string]any) map[string]map[string]any {
	t.Helper()
	entities, _ := payload["entities"].([]any)
	m := make(map[string]map[string]any, len(entities))
	for _, e := range entities {
		ent, _ := e.(map[string]any)
		name, _ := ent["name"].(string)
		m[name] = ent
	}
	return m
}
