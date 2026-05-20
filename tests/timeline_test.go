// Package tests — integration tests for the timeline MCP tool (PR-2).
//
// Covers:
//   - AC-1: Range filter (since/until) returns exactly the matching nodes in
//     DESC order with has_more:false.
//   - AC-2: Offset-based pagination delivers non-overlapping, non-gapped pages
//     with correct has_more flag on each page.
//   - AC-3: Soft-deleted nodes are excluded from timeline results.
//   - AC-4: Invalid since value produces IsError:true with an RFC3339 error
//     message before any DB query executes.
//   - AC-5: EXPLAIN shows Index Scan (not Seq Scan) on nodes_created_at_idx
//     with a corpus larger than 100 rows.
//   - AC-6: limit clamped to [1,200]; offset clamped to [0,100000]; no
//     rate-limit enforced (100 calls in-process, none return IsError).
package tests

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── seed helpers specific to timeline ─────────────────────────────────────────

// insertNodeAt inserts a node directly via raw SQL, overriding created_at to
// the given timestamp. store.Create always uses now(), so we need raw SQL to
// control the timestamp for timeline ordering tests.
// Returns the node's UUID string.
func insertNodeAt(t *testing.T, name, nodeType string, createdAt time.Time) string {
	t.Helper()
	pool := NewTestPool(t)
	ctx := context.Background()

	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO nodes (name, node_type, created_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (name) DO NOTHING
		 RETURNING id`,
		name, nodeType, createdAt,
	).Scan(&id)
	require.NoError(t, err, "insertNodeAt: insert %q at %s", name, createdAt)
	return id
}

// ── timeline call helper ───────────────────────────────────────────────────────


// extractNodeNames pulls the "name" field from each entry in the "nodes" array
// of a timeline response body, preserving order.
func extractNodeNames(t *testing.T, body map[string]any) []string {
	t.Helper()
	rawNodes, ok := body["nodes"]
	require.True(t, ok, "timeline response must contain 'nodes' key")
	nodes, ok := rawNodes.([]any)
	require.True(t, ok, "timeline.nodes must be an array, got %T", rawNodes)

	names := make([]string, 0, len(nodes))
	for i, n := range nodes {
		nm, ok := n.(map[string]any)
		require.True(t, ok, "nodes[%d] must be a JSON object, got %T", i, n)
		name, ok := nm["name"].(string)
		require.True(t, ok, "nodes[%d].name must be a string", i)
		names = append(names, name)
	}
	return names
}

// ── AC-1: range filter ─────────────────────────────────────────────────────────

// TestTimeline_RangeFilter seeds 5 nodes at T1 < T2 < T3 < T4 < T5.
// Calling timeline with since=T2 and until=T4 must return exactly [T4, T3, T2]
// in DESC order and has_more:false.
//
// AC-1: range filter returns exactly the 3 in-range nodes, ordered newest-first.
func TestTimeline_RangeFilter(t *testing.T) {
	CleanDB(t)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := base
	t2 := base.Add(1 * time.Hour)
	t3 := base.Add(2 * time.Hour)
	t4 := base.Add(3 * time.Hour)
	t5 := base.Add(4 * time.Hour)

	insertNodeAt(t, "tl-range-n1", "pattern", t1)
	insertNodeAt(t, "tl-range-n2", "pattern", t2)
	insertNodeAt(t, "tl-range-n3", "pattern", t3)
	insertNodeAt(t, "tl-range-n4", "pattern", t4)
	insertNodeAt(t, "tl-range-n5", "pattern", t5)

	c := newMCPClient(t)
	result := callTool(t, c, "timeline", map[string]any{
		"since": t2.Format(time.RFC3339),
		"until": t4.Format(time.RFC3339),
		"limit": 50,
	})

	require.False(t, result.IsError,
		"AC-1: timeline must succeed; got: %s", resultText(t, result))

	var body map[string]any
	unmarshalResult(t, result, &body)

	names := extractNodeNames(t, body)
	require.Len(t, names, 3,
		"AC-1: expected exactly 3 nodes in [T2,T4], got %d: %v", len(names), names)

	assert.Equal(t, []string{"tl-range-n4", "tl-range-n3", "tl-range-n2"}, names,
		"AC-1: nodes must be ordered newest-first (DESC)")

	hasMore, ok := body["has_more"].(bool)
	require.True(t, ok, "AC-1: has_more must be a bool, got %T", body["has_more"])
	assert.False(t, hasMore, "AC-1: has_more must be false for a fully-covered range")
}

// ── AC-2: offset-based pagination ─────────────────────────────────────────────

// TestTimeline_Pagination seeds 5 nodes at T1 < T2 < T3 < T4 < T5 and walks
// through three pages with limit=2. Asserts:
//   - Page 0 (offset 0): [T5, T4], has_more:true
//   - Page 1 (offset 2): [T3, T2], has_more:true
//   - Page 2 (offset 4): [T1],     has_more:false
//
// Also asserts no duplicates exist across all three pages combined.
//
// AC-2: pages are non-overlapping, non-gapped, with correct has_more flag.
func TestTimeline_Pagination(t *testing.T) {
	CleanDB(t)

	base := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	insertNodeAt(t, "tl-page-n1", "pattern", base.Add(0*time.Hour))
	insertNodeAt(t, "tl-page-n2", "pattern", base.Add(1*time.Hour))
	insertNodeAt(t, "tl-page-n3", "pattern", base.Add(2*time.Hour))
	insertNodeAt(t, "tl-page-n4", "pattern", base.Add(3*time.Hour))
	insertNodeAt(t, "tl-page-n5", "pattern", base.Add(4*time.Hour))

	type pageExpect struct {
		offset  int
		names   []string
		hasMore bool
	}

	pages := []pageExpect{
		{offset: 0, names: []string{"tl-page-n5", "tl-page-n4"}, hasMore: true},
		{offset: 2, names: []string{"tl-page-n3", "tl-page-n2"}, hasMore: true},
		{offset: 4, names: []string{"tl-page-n1"}, hasMore: false},
	}

	var allNames []string
	c := newMCPClient(t)

	for _, pg := range pages {
		result := callTool(t, c, "timeline", map[string]any{
			"limit":  2,
			"offset": pg.offset,
		})
		require.False(t, result.IsError,
			"AC-2: timeline page (offset=%d) must succeed: %s", pg.offset, resultText(t, result))

		var body map[string]any
		unmarshalResult(t, result, &body)

		names := extractNodeNames(t, body)
		assert.Equal(t, pg.names, names,
			"AC-2: page offset=%d names mismatch", pg.offset)

		hasMore, ok := body["has_more"].(bool)
		require.True(t, ok, "AC-2: has_more must be bool at offset=%d", pg.offset)
		assert.Equal(t, pg.hasMore, hasMore,
			"AC-2: has_more=%v expected at offset=%d", pg.hasMore, pg.offset)

		allNames = append(allNames, names...)
	}

	// Verify no duplicates across pages.
	seen := make(map[string]int)
	for i, n := range allNames {
		seen[n]++
		assert.Equal(t, 1, seen[n],
			"AC-2: duplicate node %q at combined index %d — pages must not overlap", n, i)
	}
	assert.Len(t, allNames, 5, "AC-2: all 5 nodes must appear across the three pages")
}

// ── AC-3: soft-deleted nodes excluded ─────────────────────────────────────────

// TestTimeline_ExcludesSoftDeleted seeds 3 nodes, soft-deletes 1, then asserts
// the deleted node is absent from the timeline response.
//
// AC-3: deleted_at IS NOT NULL nodes must never appear in timeline.
func TestTimeline_ExcludesSoftDeleted(t *testing.T) {
	CleanDB(t)

	base := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	insertNodeAt(t, "tl-sd-active-a", "pattern", base)
	insertNodeAt(t, "tl-sd-deleted",  "pattern", base.Add(1*time.Hour))
	insertNodeAt(t, "tl-sd-active-b", "pattern", base.Add(2*time.Hour))

	softDeleteNodeByName(t, "tl-sd-deleted")

	c := newMCPClient(t)
	result := callTool(t, c, "timeline", map[string]any{
		"since": base.Add(-1 * time.Hour).Format(time.RFC3339),
		"until": base.Add(3 * time.Hour).Format(time.RFC3339),
		"limit": 50,
	})

	require.False(t, result.IsError,
		"AC-3: timeline must succeed: %s", resultText(t, result))

	var body map[string]any
	unmarshalResult(t, result, &body)

	names := extractNodeNames(t, body)
	assert.Len(t, names, 2,
		"AC-3: only 2 active nodes must appear, got %d: %v", len(names), names)

	for _, n := range names {
		assert.NotEqual(t, "tl-sd-deleted", n,
			"AC-3: soft-deleted node must not appear in timeline results")
	}
}

// ── AC-4: invalid since format ─────────────────────────────────────────────────

// TestTimeline_BadSinceFormat invokes timeline with a non-RFC3339 since value
// and asserts the response is IsError:true containing "invalid since" or
// "RFC3339" in the error message body.
//
// AC-4: parse error returned before any DB query; structured error response.
func TestTimeline_BadSinceFormat(t *testing.T) {
	CleanDB(t)

	c := newMCPClient(t)
	result := callTool(t, c, "timeline", map[string]any{
		"since": "not-a-date",
	})

	require.True(t, result.IsError,
		"AC-4: timeline with bad since must return IsError:true")

	body := resultText(t, result)
	lowerBody := strings.ToLower(body)
	assert.True(t,
		strings.Contains(lowerBody, "invalid since") || strings.Contains(lowerBody, "rfc3339"),
		"AC-4: error message must mention 'invalid since' or 'RFC3339', got: %s", body)
}

// ── AC-5: index scan ──────────────────────────────────────────────────────────

// TestTimeline_IndexScanUsed inserts 200 nodes and runs EXPLAIN (FORMAT TEXT) on
// the ListByCreatedAt query. It asserts the query plan uses an Index Scan (not a
// Seq Scan) on nodes_created_at_idx, confirming migration 00006 is effective.
//
// Fallback: if EXPLAIN is inaccessible, we verify the index exists in pg_indexes.
//
// AC-5: partial index nodes_created_at_idx used for ORDER BY created_at DESC queries.
func TestTimeline_IndexScanUsed(t *testing.T) {
	CleanDB(t)

	pool := NewTestPool(t)
	ctx := context.Background()

	// First verify the index exists in the schema.
	var idxCount int
	err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pg_indexes
		 WHERE tablename = 'nodes' AND indexname = 'nodes_created_at_idx'`,
	).Scan(&idxCount)
	require.NoError(t, err, "AC-5: pg_indexes query must succeed")
	require.Equal(t, 1, idxCount,
		"AC-5: nodes_created_at_idx must exist (migration 00006)")

	// Seed 200 nodes so the planner prefers the index over a seq scan.
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		_, execErr := pool.Exec(ctx,
			`INSERT INTO nodes (name, node_type, created_at) VALUES ($1, $2, $3)`,
			fmt.Sprintf("tl-idx-node-%03d", i), "pattern", ts,
		)
		require.NoError(t, execErr, "AC-5: insert node %d", i)
	}

	// Run EXPLAIN (FORMAT TEXT) for the exact query used by ListByCreatedAt.
	// We omit the since/until params (both NULL) so the partial-index predicate
	// (deleted_at IS NULL) alone qualifies rows — this is the common hot path.
	explainQuery := `
		EXPLAIN (FORMAT TEXT)
		SELECT id, name, node_type
		FROM nodes
		WHERE deleted_at IS NULL
		  AND ($1::timestamptz IS NULL OR created_at >= $1)
		  AND ($2::timestamptz IS NULL OR created_at <= $2)
		ORDER BY created_at DESC, id DESC
		LIMIT $3 OFFSET $4`

	rows, err := pool.Query(ctx, explainQuery, nil, nil, 51, 0)
	require.NoError(t, err, "AC-5: EXPLAIN query must succeed")
	defer rows.Close()

	var planLines []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line), "AC-5: scan EXPLAIN line")
		planLines = append(planLines, line)
	}
	require.NoError(t, rows.Err(), "AC-5: EXPLAIN rows error")
	require.NotEmpty(t, planLines, "AC-5: EXPLAIN must return at least one line")

	plan := strings.Join(planLines, "\n")
	t.Logf("AC-5: EXPLAIN plan:\n%s", plan)

	// Accept "Index Scan" or "Index Only Scan" — both use the index.
	usesIndex := strings.Contains(plan, "Index Scan") || strings.Contains(plan, "Index Only Scan")
	assert.True(t, usesIndex,
		"AC-5: EXPLAIN must show Index Scan on nodes_created_at_idx, got plan:\n%s", plan)

	// Confirm the index name appears in the plan (rules out a different index).
	assert.True(t, strings.Contains(plan, "nodes_created_at_idx"),
		"AC-5: plan must reference nodes_created_at_idx specifically, got:\n%s", plan)
}

// ── AC-6: clamping and no rate-limit ──────────────────────────────────────────

// TestTimeline_ClampingAndReadOnly verifies two properties:
//
//  1. Clamping: inserting 250 nodes and calling timeline with limit=9999,
//     offset=-5 must cap the response at 200 nodes (the max) and return the
//     most recent node first (offset effectively 0).
//
//  2. No rate-limit: 100 successive in-process calls must all succeed (no
//     IsError=true), proving checkRateLimit is not invoked.
//
// AC-6: clamping [1,200] / [0,100000]; handler is exempt from rate-limiting.
func TestTimeline_ClampingAndReadOnly(t *testing.T) {
	CleanDB(t)

	pool := NewTestPool(t)
	ctx := context.Background()

	// Seed 250 nodes with distinct timestamps so ordering is deterministic.
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 250; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		_, err := pool.Exec(ctx,
			`INSERT INTO nodes (name, node_type, created_at) VALUES ($1, $2, $3)`,
			fmt.Sprintf("tl-clamp-node-%03d", i), "pattern", ts,
		)
		require.NoError(t, err, "AC-6: insert node %d", i)
	}

	c := newMCPClient(t)

	// ── clamping check ────────────────────────────────────────────────────────
	result := callTool(t, c, "timeline", map[string]any{
		"limit":  9999,
		"offset": -5,
	})
	require.False(t, result.IsError,
		"AC-6: timeline with out-of-range params must succeed: %s", resultText(t, result))

	var body map[string]any
	unmarshalResult(t, result, &body)

	names := extractNodeNames(t, body)
	assert.Len(t, names, 200,
		"AC-6: limit must be clamped to 200 even when caller passes 9999, got %d", len(names))

	// With offset clamped to 0, the first result must be the most recent node
	// (tl-clamp-node-249, inserted with the highest timestamp).
	if len(names) > 0 {
		assert.Equal(t, "tl-clamp-node-249", names[0],
			"AC-6: first node must be the most recent (offset effectively 0)")
	}

	nodeCount, ok := body["node_count"].(float64)
	require.True(t, ok, "AC-6: node_count must be a number")
	assert.Equal(t, float64(200), nodeCount,
		"AC-6: node_count in response must equal the clamped page size")

	// ── no rate-limit check ───────────────────────────────────────────────────
	const iterations = 100
	for i := range iterations {
		r := callTool(t, c, "timeline", map[string]any{"limit": 1})
		require.False(t, r.IsError,
			"AC-6: timeline call %d/%d must not be rate-limited: %s",
			i+1, iterations, resultText(t, r))
	}
}
