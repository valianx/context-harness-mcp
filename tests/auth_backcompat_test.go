// Package tests — back-compat integration test for PR-2 auth middleware.
// Verifies that MCP_AUTH=none lets unauthenticated requests through and that
// write tools persist rows with created_by_user_id = NULL.
package tests

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mariogutierrez/context-harness-mcp/internal/auth"
	internalmcp "github.com/mariogutierrez/context-harness-mcp/internal/mcp"
	"github.com/mariogutierrez/context-harness-mcp/internal/ratelimit"
)

// ── AC-11: MCP_AUTH=none back-compat path ─────────────────────────────────────

// TestAuthBackcompat_ModeNone_NoBearer verifies that with MCP_AUTH=none (default
// back-compat mode), a request to /mcp without an Authorization header is let
// through — no 401, no 403, the handler runs normally.
//
// AC-11: Given MCP_AUTH=none and no bearer, Then 200 (no auth rejection).
func TestAuthBackcompat_ModeNone_NoBearer(t *testing.T) {
	pool := NewTestPool(t)

	// Wire up the same way buildAuthMux does but explicitly use ModeNone.
	s := internalmcp.New(pool, ratelimit.New())
	mcpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = s
		fmt.Fprint(w, `{"jsonrpc":"2.0","result":{"content":[{"type":"text","text":"ok"}]}}`)
	})

	// ModeNone — no RevocationStore or cache needed (pass-through).
	cache := auth.NewRevocationCache()
	revStore := &revocationStoreAdapter{pool: pool}
	wrapped := auth.Middleware(auth.ModeNone, revStore, cache, "", mcpHandler)

	mux := http.NewServeMux()
	mux.Handle("/mcp", wrapped)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Request WITHOUT Authorization header.
	resp, err := http.Post(srv.URL+"/mcp", "application/json",
		bytes.NewBufferString(`{"jsonrpc":"2.0","method":"initialize","id":1}`))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"MCP_AUTH=none must allow unauthenticated requests through (AC-11)")
}

// TestAuthBackcompat_ModeNone_NullAttribution verifies that when MCP_AUTH=none
// and a node is created, the created_by_user_id column is NULL (back-compat
// attribution behavior for pre-auth rows).
//
// AC-11: Given MCP_AUTH=none, Then write tools persist rows with created_by_user_id = NULL.
func TestAuthBackcompat_ModeNone_NullAttribution(t *testing.T) {
	requireRealEmbedder(t)
	CleanDB(t)
	pool := NewTestPool(t)
	ctx := context.Background()

	// Use the in-process MCP client (no auth layer) to create a node.
	// Since the middleware is ModeNone (pass-through), the ctx will not carry
	// a user ID, and the store must persist NULL for created_by_user_id.
	c := newMCPClient(t)

	result := callTool(t, c, "create_nodes", map[string]any{
		"nodes": []map[string]any{
			{
				"name":         "backcompat-node",
				"nodeType":     "pattern",
				"observations": []string{"created without authentication"},
			},
		},
	})

	assert.False(t, result.IsError,
		"create_nodes must succeed in MCP_AUTH=none mode; error: %s", resultText(t, result))

	// Verify the DB row has created_by_user_id = NULL.
	var createdByUserID *string
	err := pool.QueryRow(ctx,
		`SELECT created_by_user_id::text
		   FROM nodes
		  WHERE name = 'backcompat-node'
		    AND deleted_at IS NULL`,
	).Scan(&createdByUserID)

	require.NoError(t, err,
		"backcompat-node must exist in DB after create_nodes (AC-11)")
	assert.Nil(t, createdByUserID,
		"created_by_user_id must be NULL for rows created with MCP_AUTH=none (AC-11)")
}
