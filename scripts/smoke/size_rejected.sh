#!/bin/sh
# scripts/smoke/size_rejected.sh
#
# Smoke test: verify that an oversized observation is rejected with
# code="policy/size-exceeded" by the syntactic layer of the Content Filter.
# Requires: curl, jq, python3
#
# Usage:
#   bash scripts/smoke/size_rejected.sh
#   MCP_URL=http://localhost:8080/mcp bash scripts/smoke/size_rejected.sh

set -e

MCP_URL="${MCP_URL:-http://localhost:8080/mcp}"
SEED_NODE="smoke-size-seed-$(date +%s)"

fail() {
    echo "FAIL: $1"
    exit 1
}

# ── helpers ───────────────────────────────────────────────────────────────────

initialize() {
    curl -fsSL -X POST "$MCP_URL" \
        -H 'Content-Type: application/json' \
        -D /tmp/smoke_size_headers.txt \
        -d '{
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2025-03-26",
                "clientInfo": {"name": "smoke-test", "version": "0.1.0"}
            }
        }' > /tmp/smoke_size_body.txt 2>/dev/null

    grep -i "Mcp-Session-Id" /tmp/smoke_size_headers.txt | tr -d '\r' | awk '{print $2}'
}

mcp_call_file() {
    session_id="$1"
    payload_file="$2"

    curl -fsSL -X POST "$MCP_URL" \
        -H 'Content-Type: application/json' \
        -H "Mcp-Session-Id: $session_id" \
        --data-binary "@$payload_file" 2>/dev/null
}

extract_text() {
    echo "$1" | jq -r '.result.content[0].text // empty'
}

# ── test ──────────────────────────────────────────────────────────────────────

echo "=== size_rejected.sh: MCP_URL=$MCP_URL ==="
echo "--- Step 1: initialize session ---"
SESSION_ID=$(initialize)
[ -z "$SESSION_ID" ] && fail "initialize did not return a session ID"
echo "session_id=$SESSION_ID"

echo "--- Step 2: create seed node ---"
# Write payload to a file to avoid shell ARG_MAX limits.
printf '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_nodes","arguments":{"nodes":[{"name":"%s","nodeType":"pattern","observations":["clean seed observation"]}]}}}' \
    "$SEED_NODE" > /tmp/smoke_seed_sz.json

SEED_RESP=$(mcp_call_file "$SESSION_ID" "/tmp/smoke_seed_sz.json")
SEED_TEXT=$(extract_text "$SEED_RESP")
CREATED=$(echo "$SEED_TEXT" | jq -r '.created_nodes // 0')
[ "$CREATED" = "1" ] || fail "failed to create seed node. Response: $SEED_RESP"
echo "seed node created"

echo "--- Step 3: build oversized payload and send ---"
# Generate a JSON payload with a 65000-char observation via python3.
# Python is available on every CI runner and developer machine in this project
# (it's used for the scripts/ migration tools). The large string cannot be
# passed as a shell variable on systems with small ARG_MAX — a temp file is
# the portable approach.
python3 - "$SEED_NODE" > /tmp/smoke_size_payload.json <<'PYEOF'
import json, sys
node_name = sys.argv[1]
big_text = "a" * 65000  # 65 KB — well above MaxObservationChars=5000
payload = {
    "jsonrpc": "2.0",
    "id": 3,
    "method": "tools/call",
    "params": {
        "name": "add_observations",
        "arguments": {
            "observations": [
                {"nodeName": node_name, "contents": [big_text]}
            ]
        }
    }
}
print(json.dumps(payload))
PYEOF

OBS_RESP=$(mcp_call_file "$SESSION_ID" "/tmp/smoke_size_payload.json")
OBS_TEXT=$(extract_text "$OBS_RESP")
echo "response text (first 200 chars): $(echo "$OBS_TEXT" | cut -c1-200)"

CODE=$(echo "$OBS_TEXT" | jq -r '.code // empty')
LAYER=$(echo "$OBS_TEXT" | jq -r '.layer // empty')
echo "code=$CODE layer=$LAYER"
[ "$CODE"  = "policy/size-exceeded" ] || fail "expected code=policy/size-exceeded, got '$CODE'"
[ "$LAYER" = "syntactic" ]            || fail "expected layer=syntactic, got '$LAYER'"

echo "PASS"
exit 0
