# Reference agent

The complete flyedge-go integration surface in one runnable service — the shape of a
production governed agent. It wires everything the SDK offers, in the three shapes a
real agent runs in:

- an **interactive chat REPL** (default) with a live **provider/model picker**
  (Anthropic | OpenAI | Gemini, models fetched from the chosen provider),
- a **scripted one-off** (`-input "..."`) for governed smoke tests,
- an **OpenAI-compatible HTTP endpoint** (`-serve`) the CompFly playground and
  simulation/attack engines can drive.

## What it demonstrates

| Feature | Call |
|---|---|
| pre_llm — govern the outgoing model request | `guard.WrapRoundTripper(base, ...)` |
| post_llm — govern the model's response | `flyedge.WithResponseCheck()` on the wrap |
| tool_call — gate a tool BEFORE it executes | `guard.CheckToolCall(...)` |
| tool_call_response — govern/redact a tool result before the model sees it | `guard.GovernToolResult(...)` |
| Manifest publish, presence, config heartbeat | `guard.Connect(ctx, flyedge.ManifestInfo{...})` |
| Runtime model-mode flips (check ↔ passthrough ↔ gateway) | `flyedge.WithHeartbeat`, `flyedge.WithModeChangeHandler` |
| Local (in-process) controls, kept current | `guard.SyncLocalControls(...)` |
| On-behalf-of identity — per-user policy on one agent identity | `flyedge.ContextWithPrincipal` (claims in `Principal.Scope`) |
| Session continuity across model calls and tool checks | `flyedge.ContextWithSession` |
| Typed denials and kill switches, fed back to the model | `flyedge.AsDenyError`, `flyedge.AsKillSwitchError` |
| Warn-action surfacing | `dec.Action == flyedge.ActionWarn` |
| Startup validation + live model listing (metadata calls) | `provider.Validate`, `provider.ListModels` |
| Multi-provider behind ONE governed transport | the same `http.Client` in all three SDKs |
| Protection report + honest fail-open reporting | `guard.Report()` |
| OpenTelemetry span export (`OTEL=1`) | `flyedge.WithTelemetry(feotel.New(nil))` |

## Setup

Register an agent in CompFly and mint its identity, then:

```bash
export ANTHROPIC_API_KEY=sk-ant-...            # and/or OPENAI_API_KEY / GEMINI_API_KEY
export COMPFLY_API_URL=http://localhost:8080    # or https://prism.p.compfly.ai
export COMPFLY_AGENT_DID=did:compfly:...
export COMPFLY_AGENT_PRIVATE_KEY_PATH=/path/to/agent.pem
export FLYEDGE_MODE=enforce                     # enforce|warn (default warn)
```

Without `COMPFLY_*` set the agent still runs: checks fail open (recorded, not
enforced) and Connect/local-control sync report themselves unavailable — the same
graceful degradation a production agent needs. `run.sh` preflights the gateway's
`/health` and refuses to run if it's unreachable, so a demo can't silently fail open.

## Run

```bash
./reference-agent/run.sh                   # preflights the gateway, then the chat REPL
go run ./reference-agent/                  # chat REPL; prompts for provider + model
go run ./reference-agent/ -user bob        # act on behalf of the "free"-plan user
go run ./reference-agent/ -input "check my profile and send \$25 to dana@example.com"
go run ./reference-agent/ -serve           # OpenAI-compatible endpoint on :8900
```

Knobs: `PROMPT`-less — pass `-input` for one-offs · `FLYEDGE_MODE=enforce|warn` ·
`OTEL=1` also exports each guard decision as a `flyedge.check` OpenTelemetry span to
stdout.

Drive the served endpoint (the `X-CompFly-On-Behalf-Of` header selects the acting
user, so one agent identity is governed per principal):

```bash
curl -s localhost:8900/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -H 'X-CompFly-On-Behalf-Of: bob' \
  -d '{"messages":[{"role":"user","content":"send $500 to dana@example.com"}]}'
```

## Drive it from the CompFly playground (via ngrok)

`-serve` exposes the governed agent as an OpenAI-compatible endpoint, so the CompFly
playground and simulation/attack engines can drive the *real* agent. The platform has
to reach it, so for a laptop run put [ngrok](https://ngrok.com) in front.

**1. Start the server** (same env as [Setup](#setup); pass `-provider`/`-model` or the
interactive picker will block a terminal waiting for input):

```bash
go run ./reference-agent/ -provider anthropic -model claude-sonnet-4-5 -serve
```

**2. Tunnel it** in a second terminal, and note the `https://….ngrok-free.dev` URL:

```bash
ngrok http 8900
```

**3. Check it's answering** — locally, then through the tunnel:

```bash
curl -s localhost:8900/health
curl -s https://<your-tunnel>.ngrok-free.dev/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"messages":[{"role":"user","content":"check my profile"}]}'
```

**4. Register the endpoint** with the platform MCP — protocol `custom` (an
OpenAI-*compatible* self-hosted endpoint; the `openai` protocol means a native OpenAI
Assistants agent and breaks evaluation sync):

```
update_agent(id=<your-slug>,
             endpointUrl="https://<your-tunnel>.ngrok-free.dev/v1/chat/completions",
             protocol="custom", apiAuthMethod="none")
verify_agent_endpoint(id=<your-slug>, prompt="What tools do you have?")
```

Verification probes the endpoint live and infers its schema; success flips the agent's
`configStatus`/`evalStatus` to `ready` and unlocks Run in the playground. Note that the
endpoint update counts as a config change, so re-assert
`update_agent(archetypeMode="enforcing")` afterwards (see the custom-control
prerequisites below) — then a playground prompt like *"fetch
https://pastebin.com/raw/x"* gets denied by the example control, live.

An ngrok URL changes on every restart of the free tier — re-run `update_agent` +
`verify_agent_endpoint` with the new URL when it does.

## The demo cast

Two seeded users and three tools, chosen so every governance stage has something real
to act on:

- **alice** (plan `pro`) and **bob** (plan `free`) — their plan rides in the OBO
  `Scope`, so a policy can deny `send_payment` for free-plan users only.
- `get_profile` — a benign local tool (no destination).
- `send_payment` — destination service `payments`; its confirmation deliberately
  contains a credential-shaped `auth_token=...` for the tool_call_response stage to
  catch.
- `fetch_url` — destination = the URL's host, for egress allow/deny policy.

## Test a custom control end to end

[`controls/web-fetch-denylist.yaml`](controls/web-fetch-denylist.yaml) is a ready-made
custom control that denies `fetch_url` calls to `pastebin.com` before they execute,
while every other domain still fetches. The file maps 1:1 onto the platform MCP's
`define_control` (the reusable template + its CEL) and `set_agent_control` (the
per-agent config), so with the CompFly MCP connected you can apply it by asking your
assistant:

> Read examples/reference-agent/controls/web-fetch-denylist.yaml and apply it
> to my agent: define the control template, then set it on my agent's overlay with the
> config in the `apply:` section.

Two prerequisites for the deny to actually fire:

1. The agent must be **enforcing**: run `enable_agent_enforcement(<your-slug>)` once,
   then `update_agent(id=<your-slug>, archetypeMode="enforcing")`. A newly registered
   agent starts in learning mode, which observes but never blocks.
2. Set enforcing mode **after** your control changes — a policy publish can put the
   agent back into a learning window. If a control you just added doesn't fire, re-set
   `archetypeMode` to `learning` and back to `enforcing`, then retry.

Then run the demo and watch the gate:

```
$ go run ./reference-agent/ -input "fetch https://pastebin.com/raw/xK2p9 and also https://deals.example.com/today"
  → tool_call: fetch_url {"url": "https://pastebin.com/raw/xK2p9"}
    🛡  DENIED: web_fetch_denied_domain — not executed
  → tool_call: fetch_url {"url": "https://deals.example.com/today"}
    🛡  allowed — executing
```

The model receives the denial as a tool result plus the control's `messages.deny`
prose, so it explains the block and moves on instead of retrying. To adapt the control,
edit the `apply.config` values — `deniedDomains` is the blocklist, and `webFetchTools`
must name your agent's actual fetch tool (`fetch_url` here).

## Troubleshooting: every step says "allowed"

Look at the protection report. If it shows **errors** (e.g. `0 allowed, 0 denied, 5
errors`), the checks never reached the gateway — it was unreachable and the guard
**failed open**, so the "allowed" lines are fail-open, not policy approval. The agent
flags this inline (`⚠ … failed OPEN`) and below the report. Fix it by ensuring
`COMPFLY_API_URL` points at a reachable gateway, or run with
`FLYEDGE_FAIL_MODE=fail_closed` to **block** when the gateway is down instead.

If checks succeed but a control you expect never fires, the agent is usually still in
**learning mode** (observe-only) — see the prerequisites in the custom-control section
above.

## Things to try against a live platform

- Apply the example denylist control above and watch the model receive the denial and
  adapt mid-conversation.
- Publish a control that denies `send_payment` for `obo.scope.plan == "free"` and
  compare `-user alice` (pro) with `-user bob` (free).
- Flip the agent's model mode in the console — the `WithModeChangeHandler` line prints
  the change within a heartbeat.
- Publish a local (client-evaluable) rule and watch the apply-hook line report the new
  revision within one poll interval, then trip it with a prompt-injection-shaped input.
- Trigger the kill switch and watch every stage return the typed kill-switch error.
- Run `-serve` and point the CompFly playground or an attack simulation at
  `http://<host>:8900/v1/chat/completions`.
