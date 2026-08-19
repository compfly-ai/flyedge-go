#!/usr/bin/env bash
# Run the flyedge-go sim-target — a Guard-wrapped agent an evaluation engine can drive.
# Config comes from the environment or a local .env file next to this script (gitignored).
# See README.md for first-time setup (registering the agent + minting its DID via the MCP).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Load ./.env if present (KEY=value lines). Keeps machine-specific paths out of the script.
if [ -f "$HERE/.env" ]; then set -a; . "$HERE/.env"; set +a; fi

: "${COMPFLY_API_URL:=https://prism.p.compfly.ai}"   # prism gateway base
: "${SIM_TARGET_ADDR:=:8899}"                        # HTTP listen address
export COMPFLY_API_URL SIM_TARGET_ADDR

if [ -z "${COMPFLY_AGENT_DID:-}" ] || [ -z "${COMPFLY_AGENT_PRIVATE_KEY_PATH:-}" ]; then
  cat <<'MSG'
✗ Missing agent identity. Set COMPFLY_AGENT_DID and COMPFLY_AGENT_PRIVATE_KEY_PATH
  (directly, or in a .env file next to this script — see .env.example).

First-time setup — register the agent and mint its DID identity:

  Via the CompFly MCP (ask Claude, or any MCP client with the compfly server):
    1. register_agent            { "slug": "my-sim-agent", "name": "My Sim Agent" }
    2. generate_agent_identity   { "id": "my-sim-agent" }
       → returns did + privateKeyPem. The private key is shown ONCE — save it:
         (paste the privateKeyPem into a file, e.g. keys/my-sim-agent.pem, chmod 600)
    3. (optional) enable_agent_enforcement { "agentSlug": "my-sim-agent" }

  Then set:
    COMPFLY_AGENT_DID=did:compfly:...
    COMPFLY_AGENT_PRIVATE_KEY_PATH=/absolute/path/to/keys/my-sim-agent.pem

  See README.md.
MSG
  exit 1
fi
export COMPFLY_AGENT_DID COMPFLY_AGENT_PRIVATE_KEY_PATH
[ -n "${COMPFLY_SIM_TELEMETRY_URL:-}" ] && export COMPFLY_SIM_TELEMETRY_URL

echo "→ sim-target starting"
echo "   API=$COMPFLY_API_URL  addr=$SIM_TARGET_ADDR"
echo "   DID=$COMPFLY_AGENT_DID"
[ -n "${COMPFLY_SIM_TELEMETRY_URL:-}" ] && echo "   telemetry override=$COMPFLY_SIM_TELEMETRY_URL"
echo "   (leave running; Ctrl-C to stop)"
cd "$HERE"
exec go run .
