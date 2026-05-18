#!/bin/sh
# scripts/smoke/ratelimit_test.sh
#
# Rate limit smoke test: sends 11 simultaneous create_entities calls in
# parallel and verifies at least one returns policy/rate-limited.
#
# Design: the token bucket allows a burst of 10. Firing 11 concurrent requests
# guarantees at least one arrives while the bucket is empty and receives a
# rate-limit rejection. Sequential calls would space out over ~11 seconds,
# giving the 1-token/second refill rate time to replenish the bucket.
#
# Requires: curl, jq

set -e
MCP_URL="${MCP_URL:-http://localhost:8080/mcp}"
TMPDIR="${TMPDIR:-/tmp}"

echo "=== ratelimit_test.sh: MCP_URL=$MCP_URL ==="

echo "--- Step 1: initialize session ---"
curl -fsSL -X POST "$MCP_URL" \
    -H 'Content-Type: application/json' \
    -D "$TMPDIR/ratelimit_headers.txt" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","clientInfo":{"name":"ratelimit-test","version":"0.1.0"}}}' \
    -o "$TMPDIR/ratelimit_init.txt" 2>/dev/null

SESSION_ID=$(grep -i "Mcp-Session-Id" "$TMPDIR/ratelimit_headers.txt" | tr -d '\r' | awk '{print $2}')
echo "session_id=$SESSION_ID"

echo "--- Step 2: 11 simultaneous create_entities calls (parallel) ---"

# Fire all 11 requests in the background simultaneously, capturing each
# response to a separate temp file.
i=1
while [ $i -le 11 ]; do
    curl -fsSL -X POST "$MCP_URL" \
        -H 'Content-Type: application/json' \
        -H "Mcp-Session-Id: $SESSION_ID" \
        -d "{\"jsonrpc\":\"2.0\",\"id\":$i,\"method\":\"tools/call\",\"params\":{\"name\":\"create_entities\",\"arguments\":{\"entities\":[{\"name\":\"rl-test-$i\",\"entityType\":\"pattern\",\"observations\":[\"rate limit test obs $i\"]}]}}}" \
        -o "$TMPDIR/ratelimit_resp_$i.json" 2>/dev/null &
    i=$((i + 1))
done

# Wait for all background curl calls to complete.
wait

echo "--- Step 3: parse responses ---"
RATE_LIMITED_COUNT=0
i=1
while [ $i -le 11 ]; do
    RESP_FILE="$TMPDIR/ratelimit_resp_$i.json"
    CODE=$(jq -r '.result.content[0].text // "{}"' "$RESP_FILE" 2>/dev/null | \
           jq -r '.code // "ok"' 2>/dev/null || echo "ok")
    echo "call $i: code=$CODE"
    if [ "$CODE" = "policy/rate-limited" ]; then
        RATE_LIMITED_COUNT=$((RATE_LIMITED_COUNT + 1))
    fi
    i=$((i + 1))
done

echo "--- Step 4: assert at least one call was rate-limited ---"
if [ "$RATE_LIMITED_COUNT" -ge 1 ]; then
    echo "PASS: $RATE_LIMITED_COUNT of 11 calls returned policy/rate-limited"
else
    echo "FAIL: expected at least 1 rate-limited response from 11 concurrent calls, got 0"
    exit 1
fi
