#!/usr/bin/env bash
# Hand-drive an ATTACK-mode simulation against a running attack-target (Terminal 2).
#
#   ./drive.sh start    # PUT an attack sim (tool_poison on a tool) to prism; prints the run id
#   ./drive.sh watch    # tail sim:telemetry:{runId} (local k3d convenience, needs kubectl)
#   ./drive.sh stop     # DELETE the sim
#
# Portable start/stop (curl to prism's internal API). Env (or .env next to this script):
#   PRISM (http://localhost:8080), INTERNAL_KEY (dev-internal-api-key), AGENT (go-sim-agent),
#   ORG, TARGET_TOOL (get_account), SOPH (2), PROTECTION_DISABLED (true), TELEMETRY_URL,
#   REDIS_NS (compfly-local — for `watch`).
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -f "$HERE/.env" ]; then set -a; . "$HERE/.env"; set +a; fi

PRISM="${PRISM:-${COMPFLY_API_URL:-http://localhost:8080}}"
INTERNAL_KEY="${INTERNAL_KEY:-dev-internal-api-key}"
AGENT="${AGENT:-go-sim-agent}"
ORG="${ORG:-66f100000000000000000001}"
TARGET_TOOL="${TARGET_TOOL:-get_account}"
SOPH="${SOPH:-2}"
PROTECTION_DISABLED="${PROTECTION_DISABLED:-true}"
TELEMETRY_URL="${TELEMETRY_URL:-${COMPFLY_SIM_TELEMETRY_URL:-ws://localhost:8080/v1/simulation/telemetry}}"
REDIS_NS="${REDIS_NS:-compfly-local}"
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
  echo "  Watch telemetry: ./drive.sh watch   ·   Stop: ./drive.sh stop"
  ;;
watch)
  [ -f "$RUNFILE" ] || { echo "no active run (run ./drive.sh start first)"; exit 1; }
  RUN_ID="$(cat "$RUNFILE")"
  echo "→ sim:telemetry:$RUN_ID (Ctrl-C to stop watching)"
  kubectl -n "$REDIS_NS" exec -it deploy/redis -- redis-cli SUBSCRIBE "sim:telemetry:$RUN_ID"
  ;;
stop)
  curl -sS -o /dev/null -w "✓ sim stopped (http %{http_code})\n" -X DELETE "$PRISM/internal/v1/agents/$AGENT/simulation" -H "x-internal-key: $INTERNAL_KEY"
  rm -f "$RUNFILE"
  ;;
*)
  echo "usage: $0 [start|watch|stop]"; exit 2 ;;
esac
