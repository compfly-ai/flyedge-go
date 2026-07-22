#!/usr/bin/env bash
# Run the attack-target agent (Terminal 1). Config from the environment or a local .env file next to
# this script (gitignored). See README.md. Then use drive.sh (Terminal 2) to start an attack-mode sim.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "$HERE/.env" ]; then set -a; . "$HERE/.env"; set +a; fi

: "${COMPFLY_API_URL:=http://localhost:8080}"
export COMPFLY_API_URL

if [ -z "${COMPFLY_AGENT_DID:-}" ] || [ -z "${COMPFLY_AGENT_PRIVATE_KEY_PATH:-}" ]; then
  cat <<'MSG'
✗ Missing agent identity. Set COMPFLY_AGENT_DID and COMPFLY_AGENT_PRIVATE_KEY_PATH
  (directly, or in a .env file next to this script — see .env.example).

First-time setup — register the agent + mint its DID identity:
  Via the CompFly MCP: register_agent {slug} → generate_agent_identity {id} (save privateKeyPem).
  Local k3d shortcut: AGENT_SLUG=<slug> bash terraform-compfly/local/scripts/register-host-agent.sh
    → writes terraform-compfly/local/keys/<slug>.{did,pem}.
See README.md.
MSG
  exit 1
fi
export COMPFLY_AGENT_DID COMPFLY_AGENT_PRIVATE_KEY_PATH
[ -n "${COMPFLY_SIM_TELEMETRY_URL:-}" ] && export COMPFLY_SIM_TELEMETRY_URL
[ -n "${ANTHROPIC_API_KEY:-}" ] && export ANTHROPIC_API_KEY

echo "→ attack-target starting  (API=$COMPFLY_API_URL  DID=$COMPFLY_AGENT_DID)"
[ -n "${ANTHROPIC_API_KEY:-}" ] && echo "   ANTHROPIC_API_KEY set → config_inject will be exercised" || echo "   no ANTHROPIC_API_KEY → tool_poison only (config_inject needs a key)"
echo "   leave running; start an attack sim with ./drive.sh start"
cd "$HERE/.."
exec go run ./attack-target
