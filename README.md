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

Go applications use this independent Go runtime. Rust applications use the
[FlyEdge Rust core](https://github.com/compfly-ai/flyedge/tree/main/crates/flyedge-core);
Python and TypeScript map to that core through PyO3 and NAPI-RS native bindings.
See the [shared-core architecture](https://github.com/compfly-ai/flyedge#one-rust-core-native-language-apis).

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

Use `WithDecisionHandler(func(CheckRequest, Decision))` at Guard construction to
observe final policy decisions, including transport warnings that allow a provider
request to continue. Read `Decision.Warnings` and `Decision.Message`; an allowed
decision can carry warnings. The optional callback runs once per `Check`, before
it returns, and must be concurrency-safe and treat its arguments as read-only.
Explicit proxy policy denials and gateway warning metadata also reach this callback.
Proxy denials return `*DenyError` with the authored `Decision.Message`; warning
metadata is delivered before the unchanged provider response, including streams.
Ordinary provider errors retain their HTTP status and body.

See the [complete developer guide](docs/DEVELOPER_GUIDE_GO.md) for Anthropic, OpenAI,
Gemini, langchaingo, tool-use loops, identity, telemetry, and production configuration.

`WrapRoundTripper` also recognizes Google Vertex `generateContent` and
`streamGenerateContent`, and Anthropic Vertex `rawPredict` and `streamRawPredict`,
on global/regional `aiplatform.googleapis.com` endpoints (`v1` and `v1beta1`).
In passthrough or gateway mode, these calls use the existing signed gateway route and
preserve gateway HTTP errors. In direct mode, non-streaming denials withhold the
response; streamed output is audited after delivery. Other Vertex operations,
such as embeddings and token counting, remain outside this transport wrapper.

OpenAI Responses (`client.Responses.New`) is covered by the same wrapped HTTP
transport, including function-call output and streaming audits. See
[Responses coverage and limits](docs/RESPONSES.md), including partial conversation
context and coordinated Prism proxy rollout and background-mode limits.

## Model routing preference

Set `Config.ModelMode` to `ModelModeCheck`, `ModelModePassthrough`, or
`ModelModeGateway`, or use `FLYEDGE_MODEL_MODE` with `LoadEnv`. Empty means inherit.
Invalid values are rejected by `New`.

Explicit per-agent platform settings take precedence over the SDK preference;
the SDK preference takes precedence over the organization default. Without either,
routing defaults to check. Servers that return a mode without recognized
`model_mode_source` metadata remain authoritative for compatibility.

Chat Completions and Responses use the same effective mode. Heartbeats and
reconnects preserve the SDK preference, and `WithModeChangeHandler` reports changes
to the effective mode. Proxy requests send `X-CompFly-Model-Mode` only for an
explicit SDK preference. The platform still applies per-agent policy and all
normal enforcement checks.

Legacy `ProxyMode: true` opts into proxy routing: it retains a non-check server
mode when present, otherwise selects passthrough. Explicit `ModelMode` takes
precedence over that legacy flag; per-agent and older-server authority still apply.

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
| `langchaingo` | Platform-invocation conversation to langchaingo messages (separate module) |
| `simulation` | simulation client and attack injection |

## Examples

Runnable programs in [`examples/`](examples/), each with its own README:

- `reference-agent` — the complete integration surface (Connect, heartbeat, local controls, all four stages, on-behalf-of identity) over standard platform invocation, with optional chat REPL, one-off, or HTTP modes, with a ready-made custom control to test enforcement
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

Platform invocation is the standard connection for Playground, simulations, and
evaluations. The agent opens an authenticated outbound WebSocket; no public HTTP
listener or tunnel is required. Follow the [Playground setup guide](docs/PLAYGROUND_SETUP.md)
for state, history, and tool-mocking walkthroughs, and the
[scenario integration guide](docs/SCENARIO_INTEGRATION.md) for handler and tool code.

The SDK sends a WebSocket Ping every 10 seconds and waits up to 30 seconds for
the matching Pong using the WebSocket library. This runs independently of HTTP
configuration polling, lease renewal, and agent work. A heartbeat failure ends
the connection without reconnecting or replaying requests. Socket errors retain
the underlying cause while remaining compatible with
`errors.Is(err, flyedge.ErrInvocationOutcomeUnknown)`.


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
| `AllowSimulationInjection` | Defaults to enabled when `nil` for platform invocation. Set a `*bool` pointing to `false` to disable injected mocks. |
| `InvocationOptions.Purposes` | Explicitly permits `PurposePlayground`, `PurposeSimulation`, and/or `PurposeEval`. |
| `LeaseDuration` | Renewable authorization window, default one hour; accepted range 1 second–24 hours. |
| `AutoRenew` | `*bool`, default true when nil. A pointer to false creates a finite lease. Renewal keeps the same connection alive until stopped or revoked. |
| `AllowOperatorReconnect` | Defaults to false. Allows an authorized Platform operator to request a fresh lease through the signed config heartbeat after the socket closes. Requires a compatible Platform deployment. |
| `AllowClientState` | `*bool`, default true when nil. A pointer to false rejects state. A declared state contract requires it to be enabled. |
| `AllowClientHistory` | Opt-in permission for caller-supplied conversation history. Map `request.History` followed by `request.Messages` into your provider conversation without re-executing past tool calls. |
| `Contract.State` | Declares `Fields` and optional `Required` names. Types: `StateString`, `StateNumber`, `StateBoolean`, `StateJSON`. No state defaults are injected. |
| `RequestTimeout` | Invocation timeout, default two minutes; accepted range 1 second–10 minutes. |
| `InvocationRequest` | Contains `ID`, `Purpose`, `Messages`, `SessionID`, `RunID`, `Deadline`, optional `State`, `History`, and `Traceparent`, and `ContractVersion`. A valid W3C v00 `Traceparent` scopes handler-context checks to the turn trace without changing `SessionID`; absent or malformed values keep the existing trace fallback. |
| `InvocationWriter.TextDelta` | Sends a text chunk immediately over the outbound WebSocket. The current platform broker collects chunks before returning the full response to Playground. |
| `InvocationConnection` | Exposes the initial lease `ID` and pinned `ContractVersion`, the current `ActiveLeaseID()`, `ExpiresAt()`, `Status()`, `Close()`, and blocking `Wait()`. |

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
error fails that invocation. With a recovery-capable broker, transport failures recover automatically. `Wait()` ends on terminal failure
and reports `ErrConnectionRevoked`, `ErrConnectionExpired`, or
`ErrInvocationOutcomeUnknown` when appropriate. With `AllowOperatorReconnect`
enabled, `Wait()` stays blocked until local shutdown. Each operator request
authorizes one fresh registration using the original invocation options and handler.
Use `connection.Status()` for health checks and diagnostics while `Wait()` is
blocked. The snapshot reports `State` (`connected`, `disconnected`, `reconnecting`,
or `closed`), the current `ActiveLeaseID`, last acknowledged `ExpiresAt`, and the
number of operator `ReconnectAttempts` (excluding initial registration).
`LastDisconnectError` and `LastReconnectError` retain the most recent errors,
including after successful recovery; use `State` to determine current connection
status. These errors support `errors.Is` where the underlying error does.
A failed operator reconnect remains disconnected until a new operator request;
the SDK does not automatically retry the same request. Expiry is historical when
disconnected or closed and must not be used alone as a readiness check.

Recovery is automatic when the broker acknowledges recovery protocol v1. No configuration switch is required. Older brokers remain usable, but cannot recover; `Status().RecoverySupported` reports this capability. Transport failures enter one serialized recovery loop.
Retries use jittered exponential backoff (0.5–1 second initially, up to 30 seconds)
and respect `Retry-After`. Temporary HTTP 409 conflicts keep a short 0.5–1 second retry delay. Signed recovery requests prove ownership with the current socket token so compatible brokers can fence the old socket immediately, without waiting for its heartbeat timeout. HTTP 409 (including a pending handshake), 429, 5xx,
and network failures are retryable. Authentication failures, missing/revoked leases,
unsupported protocols, and changed authorization are terminal. Cancellation and the
last acknowledged lease deadline bound recovery. Recovery never renews a lease,
changes its scope, or falls back to ordinary registration. It accepts a previously committed renewal even if its acknowledgement was lost. Initial registration
failures still return to the caller; this loop starts after a ready connection.

`Status().RecoveryAttempts` counts automatic attempts separately from operator
attempts; `NextRetryAt` identifies a scheduled retry. `Heartbeat` reports ping/pong
counts and timestamps, plus the last application frame, for the current socket.
It contains no request payloads. `Heartbeat.EdgeRay` records the endpoint’s Cloudflare handshake trace ID when available, so resets can be correlated with edge logs. A replacement socket starts fresh counters.

In-flight work is never replayed. Cancellation is
cooperative and cannot reverse completed side effects. Each connection serves one
active invocation at a time, with a maximum aggregate text output of 1 MiB.

The [reference concierge](examples/reference-agent/README.md) serves platform
invocations by default, with optional location state, provider conversation history,
and before-tool mock hooks. Use `go run ./reference-agent -provider openai -model <your-model>`
from `examples/` with the configured agent identity and provider key.

See [the runnable support agent](examples/remote-agent/README.md) and
[its scenario-owned state dataset](examples/remote-agent/datasets/client-state.json)
for an example with tool calls, policy checks, colored logs, and simulation telemetry.

### Optional Playground defaults

`StateField.PlaygroundDefault` is optional JSON editor metadata:

```go
"selected_order_id": {
    Type: flyedge.StateString,
    PlaygroundDefault: json.RawMessage(`"DEMO-42"`),
},
```

The SDK and Platform validate the default against the declared type. Playground
prefills it when the user enables **Supply starting state**, and preserves edits
when toggling state off and back on. No default is sent while state is disabled.
Scenarios and runtime requests never have missing fields filled automatically;
they continue to carry exactly their supplied state. Omit the default when it is
not useful. Boolean false, numeric zero, empty strings, and JSON null are retained.

Platform-invocation injection is enabled by default; endpoint integrations are unchanged.
`AllowSimulationInjection` is now `*bool`, matching the optional-state convention.
Existing callers setting it to `true` can omit it; callers disabling it should use
`disabled := false` and `AllowSimulationInjection: &disabled`. This default does not
create mocks: scenario overrides and the application's before-tool hook still control them.
