#!/bin/sh
# scripts/smoke/happy_path.sh
#
# End-to-end smoke test: create a node, verify it appears in read_graph.
# Requires: curl, jq
#
# Note: delete_nodes is not exposed as an MCP tool (admin-script only).
# Clean up test nodes with:
#   psql $SUPABASE_DB_URL -c "UPDATE nodes SET deleted_at=now() WHERE name LIKE 'smoke-test-%'"
#
# Usage:
#   bash scripts/smoke/happy_path.sh
#   MCP_URL=http://localhost:8080/mcp bash scripts/smoke/happy_path.sh

set -e

MCP_URL="${MCP_URL:-http://localhost:8080/mcp}"
NODE_NAME="smoke-test-node-$(date +%s)"

fail() {
    echo "FAIL: $1"
    exit 1
}

# ── helpers ───────────────────────────────────────────────────────────────────

# initialize establishes an MCP session and prints the session ID.
# The streamable-HTTP protocol requires an initialize handshake before tool calls.
initialize() {
    curl -fsSL -X POST "$MCP_URL" \
        -H 'Content-Type: application/json' \
        -D /tmp/smoke_init_headers.txt \
        -d '{
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2025-03-26",
                "clientInfo": {"name": "smoke-test", "version": "0.1.0"}
            }
        }' > /tmp/smoke_init_body.txt 2>/dev/null

    grep -i "Mcp-Session-Id" /tmp/smoke_init_headers.txt | tr -d '\r' | awk '{print $2}'
}

# mcp_call sends a tools/call JSON-RPC request and prints the raw result text.
mcp_call() {
    session_id="$1"
    tool_name="$2"
    arguments="$3"

    payload=$(printf '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"%s","arguments":%s}}' \
        "$tool_name" "$arguments")

    curl -fsSL -X POST "$MCP_URL" \
        -H 'Content-Type: application/json' \
        -H "Mcp-Session-Id: $session_id" \
        -d "$payload" 2>/dev/null
}

# extract_text pulls the text from the first content element of a tools/call response.
extract_text() {
    echo "$1" | jq -r '.result.content[0].text // empty'
}

# ── test ──────────────────────────────────────────────────────────────────────

echo "=== happy_path.sh: MCP_URL=$MCP_URL ==="
echo "--- Step 1: initialize session ---"
SESSION_ID=$(initialize)
[ -z "$SESSION_ID" ] && fail "initialize did not return a session ID"
echo "session_id=$SESSION_ID"

echo "--- Step 2: create_nodes ---"
CREATE_ARGS=$(printf '{"nodes":[{"name":"%s","nodeType":"pattern","observations":["smoke test observation"]}]}' \
    "$NODE_NAME")
CREATE_RESP=$(mcp_call "$SESSION_ID" "create_nodes" "$CREATE_ARGS")
CREATE_TEXT=$(extract_text "$CREATE_RESP")
[ -z "$CREATE_TEXT" ] && fail "create_nodes returned no content. Response: $CREATE_RESP"

CREATED_NODES=$(echo "$CREATE_TEXT" | jq -r '.created_nodes // 0')
CREATED_OBS=$(echo "$CREATE_TEXT" | jq -r '.created_observations // 0')
echo "created_nodes=$CREATED_NODES created_observations=$CREATED_OBS"
[ "$CREATED_NODES" = "1" ] || fail "expected created_nodes=1, got $CREATED_NODES"
[ "$CREATED_OBS" = "1" ]   || fail "expected created_observations=1, got $CREATED_OBS"

echo "--- Step 3: read_graph and assert node present ---"
GRAPH_RESP=$(mcp_call "$SESSION_ID" "read_graph" '{}')
GRAPH_TEXT=$(extract_text "$GRAPH_RESP")
[ -z "$GRAPH_TEXT" ] && fail "read_graph returned no content. Response: $GRAPH_RESP"

echo "$GRAPH_TEXT" | jq -e --arg name "$NODE_NAME" \
    '.nodes | map(select(.name == $name)) | length == 1' > /dev/null \
    || fail "node '$NODE_NAME' not found in read_graph response"
echo "node found in read_graph"

echo "PASS"
exit 0
