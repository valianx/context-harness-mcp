package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/khctl"
)

// TestSeed_Idempotent verifies that running seed twice inserts rows only the
// first time and is a no-op on the second run (ON CONFLICT DO NOTHING).
func TestSeed_Idempotent(t *testing.T) {
	if khctlPool == nil {
		t.Skip("Docker daemon not available")
	}
	t.Cleanup(func() { cleanDB(t) })

	ctx := context.Background()

	// First seed.
	nodesIn1, obsIn1, relsIn1, err := khctl.RunSeed(ctx, khctlPool, false)
	require.NoError(t, err)
	assert.Greater(t, nodesIn1, 0, "first seed must insert nodes")
	assert.Greater(t, obsIn1, 0, "first seed must insert observations")
	assert.Greater(t, relsIn1, 0, "first seed must insert relations")

	// Second seed — idempotent.
	nodesIn2, obsIn2, relsIn2, err := khctl.RunSeed(ctx, khctlPool, false)
	require.NoError(t, err)
	assert.Equal(t, 0, nodesIn2, "second seed: no new nodes")
	assert.Equal(t, 0, obsIn2, "second seed: no new observations")
	assert.Equal(t, 0, relsIn2, "second seed: no new relations")
}

// TestSeed_Reset verifies that --reset truncates all rows before seeding.
func TestSeed_Reset(t *testing.T) {
	if khctlPool == nil {
		t.Skip("Docker daemon not available")
	}
	t.Cleanup(func() { cleanDB(t) })

	ctx := context.Background()

	// Seed once without reset.
	_, _, _, err := khctl.RunSeed(ctx, khctlPool, false)
	require.NoError(t, err)

	// Count rows before reset.
	var countBefore int
	require.NoError(t, khctlPool.QueryRow(ctx,
		"SELECT COUNT(*) FROM nodes WHERE deleted_at IS NULL").Scan(&countBefore))
	assert.Greater(t, countBefore, 0)

	// Seed with reset.
	nodesIn, obsIn, relsIn, err := khctl.RunSeed(ctx, khctlPool, true)
	require.NoError(t, err)
	assert.Greater(t, nodesIn, 0, "seed with reset must insert nodes")
	assert.Greater(t, obsIn, 0, "seed with reset must insert observations")
	assert.Greater(t, relsIn, 0, "seed with reset must insert relations")
}

// TestSeed_MinimumCounts verifies the documented minimums from the brief:
// ≥20 nodes across ≥5 types, ≥10 relations, ≥3 observations per node.
func TestSeed_MinimumCounts(t *testing.T) {
	if khctlPool == nil {
		t.Skip("Docker daemon not available")
	}
	t.Cleanup(func() { cleanDB(t) })

	ctx := context.Background()
	_, _, _, err := khctl.RunSeed(ctx, khctlPool, false)
	require.NoError(t, err)

	var nodeCount, relCount, typeCount int
	require.NoError(t, khctlPool.QueryRow(ctx,
		"SELECT COUNT(*) FROM nodes WHERE deleted_at IS NULL").Scan(&nodeCount))
	require.NoError(t, khctlPool.QueryRow(ctx,
		"SELECT COUNT(*) FROM relations WHERE deleted_at IS NULL").Scan(&relCount))
	require.NoError(t, khctlPool.QueryRow(ctx,
		"SELECT COUNT(DISTINCT node_type) FROM nodes WHERE deleted_at IS NULL").Scan(&typeCount))

	assert.GreaterOrEqual(t, nodeCount, 20, "seed must produce ≥20 nodes")
	assert.GreaterOrEqual(t, relCount, 10, "seed must produce ≥10 relations")
	assert.GreaterOrEqual(t, typeCount, 5, "seed must produce ≥5 node types")

	// Verify ≥3 observations per node (min across all seeded nodes).
	var minObs int
	require.NoError(t, khctlPool.QueryRow(ctx, `
		SELECT MIN(obs_count)
		FROM (
			SELECT node_id, COUNT(*) AS obs_count
			FROM observations
			WHERE deleted_at IS NULL
			GROUP BY node_id
		) AS counts`).Scan(&minObs))
	assert.GreaterOrEqual(t, minObs, 3, "every seeded node must have ≥3 observations")
}
