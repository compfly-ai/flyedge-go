# flyedge-go

The Go edge runtime SDK for [CompFly](https://compfly.ai), providing explicit
governance hooks for AI agents. You construct a `Guard`, pass it through your
application, and route the model calls and tool operations you want governed through it.

Flyedge integrates your agent with the CompFly control plane: it sends activity for
policy evaluation, applies returned decisions, and emits telemetry. It is intentionally
not an autonomous security layer or a replacement for your application's authorization
controls.

## Install

```bash
go get github.com/compfly-ai/flyedge-go
```

Requires Go 1.23+.

## Quick start

Wrap the HTTP transport used by your model client to govern outbound model requests.
For tools, call `CheckToolCall` before execution and `CheckToolResponse` before
returning results to the model.

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

Adding `CheckToolCall` is what lets flyedge block a dangerous action rather than
merely observe it. Full walkthrough — Anthropic, OpenAI and langchaingo — in
[`docs/DEVELOPER_GUIDE_GO.md`](docs/DEVELOPER_GUIDE_GO.md).

## The four stages

Wire in only the ones you need.

| Stage | Guards | Call |
|---|---|---|
| `pre_llm` | the outgoing model request | `guard.WrapRoundTripper(base)` |
| `tool_call` | a tool the model wants to run | `guard.CheckToolCall(...)` |
| `tool_response` | a tool's output before it re-enters context | `guard.CheckToolResponse(...)` |
| `post_llm` | the model's response text | `WithResponseCheck()` or `guard.CheckModelResponse(...)` |

Only operations routed through these integration points are evaluated. Streaming output
that has already been delivered cannot be retracted.

## Configuration

`flyedge.LoadEnv()` is the single place environment is read; override fields on the
returned `Config` before calling `New`.

| Variable | Meaning |
|---|---|
| `COMPFLY_API_URL` | gateway base URL |
| `COMPFLY_AGENT_DID` | the agent's DID |
| `COMPFLY_AGENT_PRIVATE_KEY_PATH` | Ed25519 signing key (or `COMPFLY_AGENT_PRIVATE_KEY` inline) |
| `FLYEDGE_MODE` | `enforce` \| `warn` \| `audit` \| `off` — governs **local** detectors only |
| `FLYEDGE_FAIL_MODE` | `fail_open` (default) \| `fail_closed` — what happens when the gateway is unreachable |

Two things worth internalising:

**`FLYEDGE_MODE` does not soften server decisions.** A policy denial from the control
plane enforces regardless of mode. Mode governs only the SDK's local detectors.

**The default is fail-open** — availability over strictness. If the gateway is
unreachable, calls proceed and the error is recorded. Set `fail_closed` if a denial is
safer than an outage for your agent. A kill switch is the exception: it always
enforces and can never be failed open.

## Denials are values

A check returns a `Decision` and an `error`. A policy denial is a typed
`*DenyError`, not a panic and not an opaque failure — you decide whether the agent
refuses, retries, or takes another path. Kills carry the matching kill switch.

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

- `reference-agent` — a governed Claude tool-use agent, end to end against your CompFly platform
- `agent` — one governed transport wrap across the Anthropic and OpenAI SDKs
- `openai`, `gemini` — single-provider governed model + tool calls
- `docs-quickstart` — the snippets from the developer guide, compiled
- `langchaingo`, `otel`, `manual`, `tools` — framework, telemetry and low-level wiring
- `sim-target`, `attack-target` — Simulation Lab / red-team targets

## Docs

- [`docs/DEVELOPER_GUIDE_GO.md`](docs/DEVELOPER_GUIDE_GO.md) — the full guide

## License

[Apache-2.0](./LICENSE). See [NOTICE](./NOTICE) for attribution.
