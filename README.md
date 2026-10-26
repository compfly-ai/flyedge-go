# Flyedge Go SDK

**Govern AI agents at the edge, where model requests, tool calls, and data cross
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
| `FLYEDGE_CLOUD_TELEMETRY` | `true` (default) or `false`; controls cloud event delivery |
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

Cloud telemetry is enabled by default: events are batched to `/v1/flyedge/telemetry`
every five seconds and flushed on `Close()`. `Report()` remains available locally.
Set `Config.CloudTelemetry` to a `*bool` (`nil` means enabled), or set
`FLYEDGE_CLOUD_TELEMETRY=false` when using `LoadEnv()`, to disable cloud delivery.
Invalid environment values are rejected by `New` unless you replace the field.
Set `Config.TelemetryInterval` to customize the cadence; values below one second
use the five-second default. Explicit telemetry options override configuration.
Use `WithCloudTelemetry(interval)` to enable delivery and set the cadence, or
`WithoutCloudTelemetry()` for local-only recording. A `WithTelemetry(sink)` custom
sink takes precedence over cloud delivery.

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

- `reference-agent` — the complete integration surface (Connect, heartbeat, local controls, all four stages, on-behalf-of identity) as a chat REPL, one-off, or OpenAI-compatible HTTP service, with a ready-made custom control to test enforcement
- `agent` — one governed transport wrap across the Anthropic and OpenAI SDKs
- `openai`, `gemini` — single-provider governed model + tool calls
- `agentframework` — Microsoft Agent Framework Go with governed model and automatic tool calls
- `docs-quickstart` — the snippets from the developer guide, compiled
- `langchaingo`, `otel`, `manual`, `tools` — framework, telemetry and low-level wiring
- `sim-target`, `attack-target` — Simulation Lab / red-team targets

## Docs

- [`docs/DEVELOPER_GUIDE_GO.md`](docs/DEVELOPER_GUIDE_GO.md) — the full guide

## License

[Apache-2.0](./LICENSE). See [NOTICE](./NOTICE) for attribution.

### Observed call telemetry

`RecordLLMCallDetail` accepts optional request/response content, framework identity,
operation (defaults to `chat`), and `Data` metadata. Callers can preserve native
turn IDs, finish reasons, or content truncation indicators in `Data`. When a
subagent is supplied, the SDK reserves `delegated`, `subagent_id`, and
`subagent_type` for delegation attribution.

`RecordToolIODetail` accepts arguments/results and observed execution duration
in `LatencyMS`. Both call types accept `Err` for execution failures; cloud
telemetry carries it in the reserved `data.error` key. These failures do not
count as policy checks. Metadata is retained by ingestion, but dedicated UI
columns require consumer support. Callers choose which content to capture and
must populate these optional fields; the SDK does not collect it automatically.

## Explicit platform invocations

`Guard.ServeInvocations` lets Playground, simulations, and evaluations invoke an
application handler over an outbound WebSocket. Calling `New` or `Connect` does
not enable it: your application must explicitly register a handler and authorize
its purposes. The handler runs with your application's credentials, so wire policy
checks into its model and tool boundaries as you would for a normal invocation.

```go
connection, err := guard.ServeInvocations(ctx, flyedge.InvocationOptions{
    Name:        "Support agent",
    Environment: "staging",
    Purposes: []flyedge.InvocationPurpose{
        flyedge.PurposePlayground,
        flyedge.PurposeSimulation,
        flyedge.PurposeEval,
    },
    Contract: &flyedge.InvocationContract{
        State: flyedge.StateContract{
            Fields: map[string]flyedge.StateField{
                "selected_order_id": {Type: flyedge.StateString},
                "form":              {Type: flyedge.StateJSON},
            },
        },
    },
}, func(ctx context.Context, request flyedge.InvocationRequest, out flyedge.InvocationWriter) error {
    // Optional state is application input. Convert it into your own domain or
    // protobuf types, initialize test context, and run your application logic.
    var selectedOrder string
    if raw, supplied := request.State["selected_order_id"]; supplied {
        if err := json.Unmarshal(raw, &selectedOrder); err != nil {
            return err
        }
    }
    // Invoke your governed application here. TextDelta may be called repeatedly
    // as your model produces text. Returning nil completes this invocation.
    return out.TextDelta(ctx, "Selected order: " + selectedOrder)
})
if err != nil {
    return err
}
defer connection.Close()
return connection.Wait()
```

This snippet uses `encoding/json`. Configure the Guard with its DID and Ed25519
private key as in Quick start. Registration is signed, requires an existing
platform agent, and publishes the state contract before accepting invocations.
HTTPS is required; `AllowInsecureLoopback: true` permits only literal loopback HTTP
for local development. `Environment` selects an agent environment; `APIURL`
selects the local or hosted deployment.

| API | Behavior |
| --- | --- |
| `InvocationOptions.Purposes` | Explicitly permits `PurposePlayground`, `PurposeSimulation`, and/or `PurposeEval`. |
| `LeaseDuration` | Renewable authorization window, default one hour; accepted range 1 second–24 hours. |
| `AutoRenew` | `*bool`, default true when nil. A pointer to false creates a finite lease. Renewal keeps the same connection alive until stopped or revoked. |
| `AllowClientState` | `*bool`, default true when nil. A pointer to false rejects state. A declared state contract requires it to be enabled. |
| `Contract.State` | Declares `Fields` and optional `Required` names. Types: `StateString`, `StateNumber`, `StateBoolean`, `StateJSON`. No state defaults are injected. |
| `AllowSimulationInjection` | Default false. Explicitly allows simulation mutation hooks; unprotected baseline execution is not supported. |
| `RequestTimeout` | Invocation timeout, default two minutes; accepted range 1 second–10 minutes. |
| `InvocationRequest` | Contains `ID`, `Purpose`, `Messages`, `SessionID`, `RunID`, `Deadline`, optional `State`, and `ContractVersion`. |
| `InvocationWriter.TextDelta` | Sends a text chunk immediately over the reverse WebSocket. The current platform broker collects chunks before returning the full response to Playground. |
| `InvocationConnection` | Exposes the lease `ID`, pinned `ContractVersion`, `ExpiresAt()`, `Close()`, and blocking `Wait()`. |

`State` is `map[string]json.RawMessage`: nil means no state was supplied; a non-nil
empty map means explicit empty state. A dataset scenario supplies its own literal
`state` object, and Playground supplies values entered in the state editor. The
platform validates supplied values against the declared contract. Optional typed
fields should be omitted rather than set to null; required fields are checked
when state is supplied. State is not an authenticated identity or authorization
grant. Your application explicitly decides how to consume it before the agentic
layer runs. New evaluations do not select run-wide state profiles or substitute
tokens inside state.

The handler context is canceled on timeout, shutdown, or revocation. Returning an
error fails that invocation. `Wait()` reports `ErrConnectionRevoked`,
`ErrConnectionExpired`, or `ErrInvocationOutcomeUnknown` when appropriate. Lost
connections do not automatically reconnect or replay in-flight work. Platform
revocation is terminal; renewing the lease cannot undo it. Cancellation is
cooperative and cannot reverse completed side effects. Each connection serves one
active invocation at a time, with a maximum aggregate text output of 1 MiB.

See [the runnable support agent](examples/remote-agent/README.md) and
[its scenario-owned state dataset](examples/remote-agent/datasets/client-state.json)
for an example with tool calls, policy checks, colored logs, and simulation telemetry.
