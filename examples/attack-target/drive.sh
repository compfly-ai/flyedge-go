#!/usr/bin/env bash
# Hand-drive an ATTACK-mode simulation against a running attack-target (Terminal 2).
#
#   ./drive.sh start    # PUT an attack sim (tool_poison on a tool) to prism; prints the run id
#   ./drive.sh stop     # DELETE the sim
#
# Env (or .env next to this script):
#   PRISM (defaults to COMPFLY_API_URL / https://prism.p.compfly.ai), INTERNAL_KEY (your platform's
#   internal/service key — required), AGENT (your registered agent slug), ORG (your org id),
#   TARGET_TOOL (get_account), SOPH (2), PROTECTION_DISABLED (true), TELEMETRY_URL.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "$HERE/.env" ]; then set -a; . "$HERE/.env"; set +a; fi

PRISM="${PRISM:-${COMPFLY_API_URL:-https://prism.p.compfly.ai}}"
INTERNAL_KEY="${INTERNAL_KEY:?set INTERNAL_KEY to your platform internal/service key}"
AGENT="${AGENT:?set AGENT to your registered agent slug}"
ORG="${ORG:?set ORG to your org id}"
TARGET_TOOL="${TARGET_TOOL:-get_account}"
SOPH="${SOPH:-2}"
PROTECTION_DISABLED="${PROTECTION_DISABLED:-true}"
TELEMETRY_URL="${TELEMETRY_URL:-${COMPFLY_SIM_TELEMETRY_URL:-}}"
RUNFILE="$HERE/.attack_run"

case "${1:-start}" in
start)
  RUN_ID="run_$(date +%Y%m%d)_$(openssl rand -hex 6)"
  echo "$RUN_ID" > "$RUNFILE"
  curl -sS -X PUT "$PRISM/internal/v1/agents/$AGENT/simulation" \
    -H "x-internal-key: $INTERNAL_KEY" -H "Content-Type: application/json" \
    -d "{\"run_id\":\"$RUN_ID\",\"namespace_id\":\"$ORG\",\"middlewares\":[\"telemetry\",\"behavior_monitor\",\"attack_injector\"],\"jwt_ttl_secs\":14400,\"protection_disabled\":$PROTECTION_DISABLED,\"telemetry_url\":\"$TELEMETRY_URL\",\"extra\":{\"attack_injector\":{\"mode\":\"attack\",\"tier\":2,\"attack_config\":{\"chains\":[{\"name\":\"poison_$TARGET_TOOL\",\"steps\":[{\"strategy\":\"tool_poison\",\"target_component_type\":\"tool\",\"target_component_name\":\"$TARGET_TOOL\",\"sophistication\":$SOPH}]}]}}}}" \
    | python3 -c "import json,sys;d=json.load(sys.stdin);print('✓ attack sim active: run=%s tool_poison→%s L%s (protection_disabled=%s)'%(d.get('run_id'),'$TARGET_TOOL','$SOPH','$PROTECTION_DISABLED'))" 2>&1 || { echo "✗ PUT failed"; exit 1; }
  echo "  Terminal 1 should log 'get_account result MUTATED by injector' within ~3s."
  echo "  Watch telemetry in the CompFly platform (Simulation Lab)   ·   Stop: ./drive.sh stop"
  ;;
stop)
  curl -sS -o /dev/null -w "✓ sim stopped (http %{http_code})\n" -X DELETE "$PRISM/internal/v1/agents/$AGENT/simulation" -H "x-internal-key: $INTERNAL_KEY"
  rm -f "$RUNFILE"
  ;;
*)
  echo "usage: $0 [start|stop]"; exit 2 ;;
esac
