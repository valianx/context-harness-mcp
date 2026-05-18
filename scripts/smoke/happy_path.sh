#!/bin/sh
# scripts/smoke/happy_path.sh
#
# End-to-end smoke test: create an entity, verify it appears in read_graph, clean up.
# Requires: curl, jq
#
# Usage:
#   bash scripts/smoke/happy_path.sh
#   MCP_URL=http://localhost:8080/mcp bash scripts/smoke/happy_path.sh

set -e

MCP_URL="${MCP_URL:-http://localhost:8080/mcp}"
ENTITY_NAME="smoke-test-entity-$(date +%s)"

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

echo "--- Step 2: create_entities ---"
CREATE_ARGS=$(printf '{"entities":[{"name":"%s","entityType":"pattern","observations":["smoke test observation"]}]}' \
    "$ENTITY_NAME")
CREATE_RESP=$(mcp_call "$SESSION_ID" "create_entities" "$CREATE_ARGS")
CREATE_TEXT=$(extract_text "$CREATE_RESP")
[ -z "$CREATE_TEXT" ] && fail "create_entities returned no content. Response: $CREATE_RESP"

CREATED_ENTITIES=$(echo "$CREATE_TEXT" | jq -r '.created_entities // 0')
CREATED_OBS=$(echo "$CREATE_TEXT" | jq -r '.created_observations // 0')
echo "created_entities=$CREATED_ENTITIES created_observations=$CREATED_OBS"
[ "$CREATED_ENTITIES" = "1" ] || fail "expected created_entities=1, got $CREATED_ENTITIES"
[ "$CREATED_OBS" = "1" ]      || fail "expected created_observations=1, got $CREATED_OBS"

echo "--- Step 3: read_graph and assert entity present ---"
GRAPH_RESP=$(mcp_call "$SESSION_ID" "read_graph" '{}')
GRAPH_TEXT=$(extract_text "$GRAPH_RESP")
[ -z "$GRAPH_TEXT" ] && fail "read_graph returned no content. Response: $GRAPH_RESP"

echo "$GRAPH_TEXT" | jq -e --arg name "$ENTITY_NAME" \
    '.entities | map(select(.name == $name)) | length == 1' > /dev/null \
    || fail "entity '$ENTITY_NAME' not found in read_graph response"
echo "entity found in read_graph"

echo "--- Step 4: delete_entities (cleanup) ---"
DELETE_ARGS=$(printf '{"entityNames":["%s"]}' "$ENTITY_NAME")
DELETE_RESP=$(mcp_call "$SESSION_ID" "delete_entities" "$DELETE_ARGS")
DELETE_TEXT=$(extract_text "$DELETE_RESP")
DELETED=$(echo "$DELETE_TEXT" | jq -r '.deleted // 0')
echo "deleted=$DELETED"

echo "PASS"
exit 0
