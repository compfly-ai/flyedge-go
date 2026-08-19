#!/usr/bin/env bash
# Run the attack-target agent. Config from the environment or a local .env file next to
# this script (gitignored). See README.md. Then launch an attack-mode simulation against it
# from the CompFly Simulation Lab.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "$HERE/.env" ]; then set -a; . "$HERE/.env"; set +a; fi

: "${COMPFLY_API_URL:=https://prism.p.compfly.ai}"
export COMPFLY_API_URL

if [ -z "${COMPFLY_AGENT_DID:-}" ] || [ -z "${COMPFLY_AGENT_PRIVATE_KEY_PATH:-}" ]; then
  cat <<'MSG'
✗ Missing agent identity. Set COMPFLY_AGENT_DID and COMPFLY_AGENT_PRIVATE_KEY_PATH
  (directly, or in a .env file next to this script — see .env.example).

First-time setup — register the agent + mint its DID identity in the CompFly platform:
  Via the CompFly MCP: register_agent {slug} → generate_agent_identity {id} (save privateKeyPem).
See README.md.
MSG
  exit 1
fi
export COMPFLY_AGENT_DID COMPFLY_AGENT_PRIVATE_KEY_PATH
[ -n "${COMPFLY_SIM_TELEMETRY_URL:-}" ] && export COMPFLY_SIM_TELEMETRY_URL
[ -n "${ANTHROPIC_API_KEY:-}" ] && export ANTHROPIC_API_KEY

echo "→ attack-target starting  (API=$COMPFLY_API_URL  DID=$COMPFLY_AGENT_DID)"
[ -n "${ANTHROPIC_API_KEY:-}" ] && echo "   ANTHROPIC_API_KEY set → config_inject will be exercised" || echo "   no ANTHROPIC_API_KEY → tool_poison only (config_inject needs a key)"
echo "   leave running; launch an attack-mode simulation from the CompFly Simulation Lab"
cd "$HERE/.."
exec go run ./attack-target
