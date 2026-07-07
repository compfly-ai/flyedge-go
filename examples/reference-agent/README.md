# reference-agent — the Go guard end to end

A complete, runnable Claude tool-use agent governed by the flyedge Go guard against your **local
Compfly platform**. It's the "see it in practice" demo: watch the guard decide on every model call
and every tool call, allow the safe actions, deny the risky one, and let the agent adapt.

The agent has three tools:

| tool | destination | decision |
|---|---|---|
| `lookup_order` | local | allowed |
| `get_weather` | local | allowed |
| `fetch_url` | external egress | **decided by your agent's policy** |

The default task makes Claude use all three. **The tool-call decisions are made server-side by
prism for the agent identity you run as — not by the SDK.** For an agent whose service-access policy
restricts egress (e.g. the MCP-minted demo `kyc-risk-agent`,
`did:compfly:66f100:1fbad81e2c302b7b69b936e44e0f5c9e`), `fetch_url` is **denied**
(`external_service_denied`) and Claude finishes without the blocked data. For an agent whose policy
permits egress, `fetch_url` is **allowed** and the agent prints a `note:` saying so — that's the
platform's real decision, not a bug.

**Check the banner's `agent:` line to confirm which identity you're running as.** If it isn't the DID
above, your shell likely has `COMPFLY_AGENT_DID`/`COMPFLY_AGENT_PRIVATE_KEY_PATH` set and those
override `run.sh`'s defaults — `unset` them, or run as the demo agent, to see the deny.

## Prerequisites

- **prism reachable at `COMPFLY_API_URL`.** This matters: the guard fails **open** by default, so if the
  gateway is unreachable every check errors and is *allowed through unenforced* — the run will look
  like it "allowed" everything. Port-forward it first:
  `kubectl port-forward -n compfly-local svc/prism 8080:8080`. `run.sh` preflights `/health` and
  refuses to run if it's down.
- An agent identity (DID + Ed25519 key). The demo uses the MCP-minted one at `~/flyedge-local-demo/`.
- `ANTHROPIC_API_KEY`.

## Run

```bash
./run.sh
```

Or explicitly:

```bash
COMPFLY_API_URL=http://localhost:8080 \
COMPFLY_AGENT_DID=$(cat ~/flyedge-local-demo/agent.did) \
COMPFLY_AGENT_PRIVATE_KEY_PATH=$HOME/flyedge-local-demo/agent_key.pem \
ANTHROPIC_API_KEY=sk-ant-... \
FLYEDGE_MODE=enforce \
go run ./reference-agent/
```

Knobs: `PROMPT` overrides the task · `FLYEDGE_MODE=enforce|warn` · `OTEL=1` also exports each guard
decision as a `flyedge.check` OpenTelemetry span to stdout.

## What a run looks like

```
── turn 1 ──────────────────────────────────────────
  🛡  pre_llm allowed
  claude: I'll help you get the order status, weather, and shipping updates...
  → tool_call: lookup_order(order_id=A1023)
    🛡  allowed — executing
    result: order A1023: status=IN_TRANSIT, carrier=DHL, eta=2 days
  → tool_call: get_weather(city=Paris)
    🛡  allowed — executing
  → tool_call: fetch_url(url=https://tracking.example.com/orders/A1023)
    🛡  DENIED: external_service_denied — not executed
── turn 2 ──────────────────────────────────────────
  🛡  pre_llm allowed
  claude: Here's a summary ... I wasn't able to access the tracking page due to security restrictions ...

── protection report ───────────────────────────────
flyedge: 5 checks — 4 allowed, 1 denied, 0 warned, 0 errors
```

## Troubleshooting: every step says "allowed"

Look at the protection report. If it shows **errors** (e.g. `0 allowed, 0 denied, 5 errors`), the
checks never reached prism — the gateway was unreachable and the guard **failed open**, so the
"allowed" lines are fail-open, not policy approval. The agent flags this inline (`⚠ … failed OPEN`)
and at the end. Fix it by port-forwarding prism, or run with `FLYEDGE_FAIL_MODE=fail_closed` to
**block** when the gateway is down instead of allowing.

A clean enforced run shows `0 errors` (e.g. `4 allowed, 1 denied, 0 errors`).

## How the guard is wired

- **pre_llm** — `guard.WrapRoundTripper` installs one governed `http.Client` into the Anthropic SDK;
  every model call is checked before it leaves. A denial surfaces as a `*flyedge.DenyError`.
- **tool_call** — before running any tool the agent calls `guard.CheckToolCall(...)`; on deny it
  returns the denial to the model as a tool result so the loop continues.
- **telemetry** — `guard.Report()` prints the local aggregate; `OTEL=1` additionally exports spans.
