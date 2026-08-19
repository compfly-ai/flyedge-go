#!/usr/bin/env bash
# Run the reference agent against your CompFly platform.
# Prereqs: COMPFLY_AGENT_DID and COMPFLY_AGENT_PRIVATE_KEY_PATH set (register an agent and mint its
# identity in the CompFly platform), ANTHROPIC_API_KEY set, and COMPFLY_API_URL pointing at a
# reachable gateway (defaults to https://prism.p.compfly.ai when unset).
set -euo pipefail
cd "$(dirname "$0")/.."

export COMPFLY_API_URL="${COMPFLY_API_URL:-https://prism.p.compfly.ai}"
export FLYEDGE_MODE="${FLYEDGE_MODE:-enforce}"

if [ -z "${COMPFLY_AGENT_DID:-}" ] || [ -z "${COMPFLY_AGENT_PRIVATE_KEY_PATH:-}" ]; then
	echo "✗ Missing agent identity. Set COMPFLY_AGENT_DID and COMPFLY_AGENT_PRIVATE_KEY_PATH." >&2
	echo "  Register an agent in the CompFly platform and mint its DID + Ed25519 key, then:" >&2
	echo "    export COMPFLY_AGENT_DID=did:compfly:..." >&2
	echo "    export COMPFLY_AGENT_PRIVATE_KEY_PATH=/path/to/agent.pem" >&2
	exit 1
fi
export COMPFLY_AGENT_DID COMPFLY_AGENT_PRIVATE_KEY_PATH

# Preflight: the guard fails OPEN by default, so an unreachable gateway silently allows everything.
# Catch it here rather than letting the demo look like it "allowed" every action.
if ! curl -fsS -m 3 "$COMPFLY_API_URL/health" >/dev/null 2>&1; then
	echo "✗ gateway $COMPFLY_API_URL is not reachable." >&2
	echo "  Ensure COMPFLY_API_URL points at a reachable CompFly gateway" >&2
	echo "  (or set FLYEDGE_FAIL_MODE=fail_closed to block instead of failing open.)" >&2
	exit 1
fi
echo "✓ gateway reachable: $COMPFLY_API_URL   agent: $COMPFLY_AGENT_DID"

exec go run ./reference-agent/
