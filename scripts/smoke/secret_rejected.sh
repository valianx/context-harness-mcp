#!/bin/sh
# scripts/smoke/secret_rejected.sh
#
# Smoke test: verify that an observation containing an AWS-shaped access key
# is rejected with code="policy/secret-detected" by the Content Filter.
# Requires: curl, jq
#
# Usage:
#   bash scripts/smoke/secret_rejected.sh
#   MCP_URL=http://localhost:8080/mcp bash scripts/smoke/secret_rejected.sh
#
# Note on the test credential: the string AKIAIOSFODNN7EXAMPLE is the canonical
# AWS access key ID example from AWS's own documentation. It matches the inline
# regex pattern AKIA[0-9A-Z]{16} in internal/validate/secrets.go and is on the
# public allowlist of real secret scanners (no live credentials exist for it).

set -e

MCP_URL="${MCP_URL:-http://localhost:8080/mcp}"
SEED_ENTITY="smoke-secret-seed-$(date +%s)"

fail() {
    echo "FAIL: $1"
    exit 1
}

# ── helpers ───────────────────────────────────────────────────────────────────

initialize() {
    curl -fsSL -X POST "$MCP_URL" \
        -H 'Content-Type: application/json' \
        -D /tmp/smoke_secret_headers.txt \
        -d '{
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2025-03-26",
                "clientInfo": {"name": "smoke-test", "version": "0.1.0"}
            }
        }' > /tmp/smoke_secret_body.txt 2>/dev/null

    grep -i "Mcp-Session-Id" /tmp/smoke_secret_headers.txt | tr -d '\r' | awk '{print $2}'
}

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

extract_text() {
    echo "$1" | jq -r '.result.content[0].text // empty'
}

# ── test ──────────────────────────────────────────────────────────────────────

echo "=== secret_rejected.sh: MCP_URL=$MCP_URL ==="
echo "--- Step 1: initialize session ---"
SESSION_ID=$(initialize)
[ -z "$SESSION_ID" ] && fail "initialize did not return a session ID"
echo "session_id=$SESSION_ID"

echo "--- Step 2: create seed entity ---"
SEED_ARGS=$(printf '{"entities":[{"name":"%s","entityType":"pattern","observations":["clean seed observation"]}]}' \
    "$SEED_ENTITY")
SEED_RESP=$(mcp_call "$SESSION_ID" "create_entities" "$SEED_ARGS")
SEED_TEXT=$(extract_text "$SEED_RESP")
CREATED=$(echo "$SEED_TEXT" | jq -r '.created_entities // 0')
[ "$CREATED" = "1" ] || fail "failed to create seed entity. Response: $SEED_RESP"
echo "seed entity created"

echo "--- Step 3: add_observations with AWS key — expect policy/secret-detected ---"
# AKIAIOSFODNN7EXAMPLE matches AKIA[0-9A-Z]{16} (canonical AWS docs example key).
OBS_ARGS=$(printf '{"observations":[{"entityName":"%s","contents":["Found AKIAIOSFODNN7EXAMPLE in environment"]}]}' \
    "$SEED_ENTITY")
OBS_RESP=$(mcp_call "$SESSION_ID" "add_observations" "$OBS_ARGS")
OBS_TEXT=$(extract_text "$OBS_RESP")
echo "response text: $OBS_TEXT"

CODE=$(echo "$OBS_TEXT" | jq -r '.code // empty')
LAYER=$(echo "$OBS_TEXT" | jq -r '.layer // empty')
echo "code=$CODE layer=$LAYER"
[ "$CODE"  = "policy/secret-detected" ] || fail "expected code=policy/secret-detected, got '$CODE'"
[ "$LAYER" = "secrets" ]                || fail "expected layer=secrets, got '$LAYER'"

echo "--- Step 4: delete seed entity (cleanup) ---"
CLEANUP_ARGS=$(printf '{"entityNames":["%s"]}' "$SEED_ENTITY")
mcp_call "$SESSION_ID" "delete_entities" "$CLEANUP_ARGS" > /dev/null

echo "PASS"
exit 0
