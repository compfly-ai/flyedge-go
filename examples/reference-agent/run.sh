#!/usr/bin/env bash
# Run the reference agent against the local platform using the MCP-minted demo identity.
# Prereqs: prism port-forwarded to :8080, ~/flyedge-local-demo/ populated, ANTHROPIC_API_KEY set
# (or present in ~/flyedge-local-demo/anthropic.env).
set -euo pipefail
cd "$(dirname "$0")/.."

DEMO="$HOME/flyedge-local-demo"
[ -f "$DEMO/anthropic.env" ] && set -a && . "$DEMO/anthropic.env" && set +a

export COMPFLY_API_URL="${COMPFLY_API_URL:-http://localhost:8080}"
export COMPFLY_AGENT_DID="${COMPFLY_AGENT_DID:-$(cat "$DEMO/agent.did")}"
export COMPFLY_AGENT_PRIVATE_KEY_PATH="${COMPFLY_AGENT_PRIVATE_KEY_PATH:-$DEMO/agent_key.pem}"
export FLYEDGE_MODE="${FLYEDGE_MODE:-enforce}"

# Preflight: the guard fails OPEN by default, so an unreachable gateway silently allows everything.
# Catch it here rather than letting the demo look like it "allowed" every action.
if ! curl -fsS -m 3 "$COMPFLY_API_URL/health" >/dev/null 2>&1; then
	echo "✗ gateway $COMPFLY_API_URL is not reachable — port-forward prism first:" >&2
	echo "    kubectl port-forward -n compfly-local svc/prism 8080:8080" >&2
	echo "  (or set FLYEDGE_FAIL_MODE=fail_closed to block instead of failing open.)" >&2
	exit 1
fi
echo "✓ gateway reachable: $COMPFLY_API_URL   agent: $COMPFLY_AGENT_DID"

exec go run ./reference-agent/
