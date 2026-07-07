#!/usr/bin/env bash
# Send an Anthropic Messages request THROUGH the flyedge-proxy: agent → proxy (flyedge pre_llm
# check via prism) → api.anthropic.com. Benign → Claude replies; policy Deny → 403.
#
# Prereq: flyedge-proxy running on :9000 (see README) and ANTHROPIC_API_KEY set.
set -euo pipefail
PROXY="${PROXY_URL:-http://localhost:9000}"
PROMPT="${1:-What are your store hours?}"
: "${ANTHROPIC_API_KEY:?set ANTHROPIC_API_KEY}"

code=$(curl -s -o /tmp/flyedge_proxy_resp.json -w "%{http_code}" -X POST "$PROXY/v1/messages" \
  -H "x-api-key: $ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "content-type: application/json" \
  -H "X-Session-Id: proxy-demo" \
  -d "{\"model\":\"claude-haiku-4-5\",\"max_tokens\":256,\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}")

echo "HTTP $code"
if [ "$code" = "403" ]; then
  echo "BLOCKED by policy:"; cat /tmp/flyedge_proxy_resp.json
else
  python3 -c "import sys,json;d=json.load(open('/tmp/flyedge_proxy_resp.json'));print('claude:', ''.join(b.get('text','') for b in d.get('content',[])) or d)" 2>/dev/null \
    || cat /tmp/flyedge_proxy_resp.json
fi
