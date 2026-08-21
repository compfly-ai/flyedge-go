# Flyedge Go SDK

**Govern AI agents at the edge—where model requests, tool calls, and data cross
application boundaries.**

Flyedge is the Go edge runtime SDK for [CompFly](https://compfly.ai). It connects
your agent to the CompFly control plane and applies policy at runtime, before governed
model requests and tool calls execute. Use it to enforce allow, warn, or deny decisions,
honor remote kill switches, and produce auditable runtime telemetry.

The SDK is deliberately explicit and idiomatic Go: construct a `Guard`, pass it through
your application, and wire it into the boundaries you want governed. Policy denials are
typed errors. There is no global singleton, import-time side effect, or framework-specific
monkeypatching.

## How it works

```text
                         policy checks and decisions
Your agent ──▶ Flyedge Guard ◀────────────────────────▶ CompFly control plane
                    │
                    ├── allowed model request ────────▶ model provider
                    └── allowed tool call ────────────▶ tool or service
```

By default, Flyedge checks policy out of band and sends allowed model requests directly
to the provider. Proxy mode is available when model traffic must pass through the CompFly
gateway. Tool calls remain explicit because your application owns the agent loop and the
moment a tool is executed.

## Install

```bash
go get github.com/compfly-ai/flyedge-go
```

Requires Go 1.23+.

## Quick start

Register an agent in CompFly, then provide its DID and Ed25519 signing key:

```bash
export COMPFLY_AGENT_DID="did:compfly:..."
export COMPFLY_AGENT_PRIVATE_KEY_PATH="/path/to/agent-key.pem"
```

Wrap the HTTP transport used by your model client to govern outbound requests:

```go
guard, err := flyedge.New(flyedge.LoadEnv())
if err != nil {
    return err
}
defer guard.Close()

hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
client := anthropic.NewClient(
    anthropicopt.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
    anthropicopt.WithHTTPClient(hc),
)

ctx := flyedge.ContextWithSession(context.Background(), "demo-session")
resp, err := client.Messages.New(ctx, params) // a denial surfaces as a typed error
```

Call `CheckToolCall` before executing a tool and `CheckToolResponse` before returning
its result to the model. This is what lets Flyedge stop a dangerous action instead of
merely observing it.

See the [complete developer guide](docs/DEVELOPER_GUIDE_GO.md) for Anthropic, OpenAI,
Gemini, langchaingo, tool-use loops, identity, telemetry, and production configuration.

## Governance points

Wire in only the boundaries you need:

| Stage | Guards | Call |
|---|---|---|
| `pre_llm` | the outgoing model request | `guard.WrapRoundTripper(base)` |
| `tool_call` | a tool the model wants to run | `guard.CheckToolCall(...)` |
| `tool_call_response` | a tool's output before it re-enters context | `guard.CheckToolResponse(...)` |
| `post_llm` | the model's response text | `WithResponseCheck()` or `guard.CheckModelResponse(...)` |

Flyedge governs only operations routed through these integration points. Keep your
application's normal authentication and authorization in place; Flyedge adds runtime
policy at the model and tool boundary. A buffered model response can be blocked before
delivery. Streaming output is observed when the stream completes and cannot be retracted.

## Configuration

`flyedge.LoadEnv()` is the single place environment is read; override fields on the
returned `Config` before calling `New`.

| Variable | Meaning |
|---|---|
| `COMPFLY_API_URL` | gateway base URL; defaults to `https://prism.p.compfly.ai` |
| `COMPFLY_AGENT_DID` | the agent's DID |
| `COMPFLY_AGENT_PRIVATE_KEY_PATH` | Ed25519 signing key (or `COMPFLY_AGENT_PRIVATE_KEY` inline) |
| `FLYEDGE_MODE` | `enforce` \| `warn` (default) \| `audit` \| `off` |
| `FLYEDGE_FAIL_MODE` | `fail_open` (default) \| `fail_closed` — what happens when the gateway is unreachable |

The posture settings are intentionally separate:

- In `warn` and `audit`, platform warnings are advisory. In `enforce`, a warning blocks.
- A platform denial or kill-switch decision blocks in every checking mode.
- `off` bypasses policy checks entirely and is intended for local development.
- Local detectors have a separate posture, supplied by CompFly or configured in process.
  They can add a fast local denial but cannot override one from the control plane.

The default is `fail_open`: if the gateway is unreachable, the action proceeds and the
error is recorded. Set `fail_closed` when blocking is safer than continuing during an
outage. A kill-switch decision always blocks and cannot be failed open.

Protection events are summarized in memory by default. Use `WithCloudTelemetry` to send
SDK telemetry to CompFly, or use `telemetry/otel` to export checks to your observability
stack.

## Denials are values

A check returns a `Decision` and an `error`. A policy denial is a typed `*DenyError`,
not a panic or an opaque failure. Your agent decides whether to refuse, retry, or take
another path. Kill switches surface separately as `*KillSwitchError`.

## Packages

| Package | Purpose |
|---|---|
| `flyedge` | the `Guard`, config, stages, sessions |
| `enforce` | the wire contract and enforcement client |
| `identity` | DID + Ed25519 request signing |
| `telemetry` | telemetry sinks and the protection report |
| `telemetry/otel` | OpenTelemetry sink (separate module) |
| `simulation` | simulation client and attack injection |

## Examples

Runnable programs in [`examples/`](examples/), each with its own README:

- `reference-agent` — a governed Claude tool-use agent, end to end against CompFly
- `agent` — one governed transport wrap across the Anthropic and OpenAI SDKs
- `openai`, `gemini` — single-provider governed model + tool calls
- `docs-quickstart` — the snippets from the developer guide, compiled
- `langchaingo`, `otel`, `manual`, `tools` — framework, telemetry and low-level wiring
- `sim-target`, `attack-target` — Simulation Lab / red-team targets

## Docs

- [`docs/DEVELOPER_GUIDE_GO.md`](docs/DEVELOPER_GUIDE_GO.md) — the full guide

## License

[Apache-2.0](./LICENSE). See [NOTICE](./NOTICE) for attribution.
