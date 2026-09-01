# Flyedge Developer Guide — Go

Flyedge is CompFly's runtime governance layer for AI agents. It sits between your
agent and the model/tools it uses, and enforces policy on every model call and
tool call — allow, warn, or deny — while streaming telemetry to the CompFly
platform.

This guide covers the **Go SDK** (`flyedge-go`).

## A note on the Go SDK's design

The Go SDK is deliberately **explicit** ("gothonic"): no global singleton, no
import-time side effects, no monkeypatching. Instead you:

1. Construct a `*flyedge.Guard` and hold the handle.
2. Wrap your HTTP transport with `guard.WrapRoundTripper(...)` so model calls are
   governed.
3. Call `guard.CheckToolCall(...)` before you execute a tool.
4. `defer guard.Close()` to flush telemetry and stop background goroutines.

Denials come back as **typed errors** (`*flyedge.DenyError`), not exceptions or
sentinel returns. Fail-open vs fail-closed is an explicit configuration choice,
not a hidden default. This maps cleanly onto Go's error-as-value model and makes
the governance boundary visible in your code.

Concretely, that means:

- `guard.WrapRoundTripper(base)` wraps your `http.Client` — every model call over it is governed.
- A `*flyedge.Guard` handle you construct and pass around — no global singleton.
- `context.Context` carries session scope (`flyedge.ContextWithSession`).
- Denials are a typed `Decision` + `*flyedge.DenyError`, not a raise or sentinel return.
- `FailMode` is an explicit choice (`fail_open` default), not a hidden default.
- Background goroutines are owned and stopped by `guard.Close()`.

---

## Table of Contents

- [Installation](#installation)
- [Quick Start](#quick-start)
- [Core Concepts](#core-concepts)
  - [The Guard](#the-guard)
  - [The four stages](#the-four-stages)
  - [Decisions and denials](#decisions-and-denials)
- [Governing Model Calls](#governing-model-calls)
- [Governing Tool Calls](#governing-tool-calls)
- [Selective Protection](#selective-protection)
- [Sessions](#sessions)
- [Protection Modes](#protection-modes)
- [Agent Identity & CompFly Cloud](#agent-identity--compfly-cloud)
- [Manifest Registration](#manifest-registration)
- [Telemetry & OpenTelemetry](#telemetry--opentelemetry)
- [Error Handling](#error-handling)
- [Configuration](#configuration)
- [Environment Variables](#environment-variables)
- [Graceful Shutdown](#graceful-shutdown)
- [Complete Example](#complete-example)
- [API Reference](#api-reference)

---

## Installation

```bash
go get github.com/compfly-ai/flyedge-go
```

The core module keeps its dependency surface small: it uses `coder/websocket`
for the simulation telemetry channel. The optional OpenTelemetry sink lives in
a separate module (`.../flyedge-go/telemetry/otel`), so the OTel SDK only enters
your build when you import it.

```go
import (
    "github.com/compfly-ai/flyedge-go"
)
```

Requires Go 1.23+.

---

## Quick Start

### Anthropic (tool use)

The most common shape: a Claude agent that calls tools. Flyedge governs both the
model calls (via the HTTP transport) and each tool call (explicitly, before you
run it).

```go
package main

import (
    "context"
    "fmt"
    "net/http"
    "os"

    "github.com/anthropics/anthropic-sdk-go"
    anthropicopt "github.com/anthropics/anthropic-sdk-go/option"

    "github.com/compfly-ai/flyedge-go"
)

func main() {
    // 1. Build the Guard from the environment (DID, key, prism URL, mode).
    guard, err := flyedge.New(flyedge.LoadEnv())
    if err != nil {
        fmt.Fprintln(os.Stderr, "flyedge:", err)
        os.Exit(1)
    }
    defer guard.Close()

    // 2. Install ONE governed http.Client. Every model call over this transport
    //    is checked (the pre_llm stage) before it leaves the process.
    hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
    client := anthropic.NewClient(
        anthropicopt.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
        anthropicopt.WithHTTPClient(hc),
    )

    ctx := flyedge.ContextWithSession(context.Background(), "demo-session")

    resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
        Model:     anthropic.ModelClaudeSonnet4_5,
        MaxTokens: 1024,
        Messages: []anthropic.MessageParam{
            anthropic.NewUserMessage(anthropic.NewTextBlock("What's the weather in Paris?")),
        },
    })
    if err != nil {
        // A policy denial surfaces as a typed error on the model call.
        if de, ok := flyedge.AsDenyError(err); ok {
            fmt.Println("blocked by policy:", de.Decision.Reason)
            return
        }
        panic(err)
    }

    // 3. Before executing any tool the model asked for, check it.
    for _, block := range resp.Content {
        if tu := block.AsToolUse(); tu.Name != "" {
            dec, err := guard.CheckToolCall(ctx, "demo-session", tu.Name, string(tu.Input), "api.weather.com")
            if err != nil {
                if de, ok := flyedge.AsDenyError(err); ok {
                    fmt.Printf("tool %q denied: %s\n", tu.Name, de.Decision.Reason)
                    continue
                }
                panic(err)
            }
            _ = dec // dec.Action is "allow" / "warn"
            // ... execute the tool ...
        }
    }

    // 4. Print the protection report.
    fmt.Println(guard.Report())
}
```

### OpenAI (tool use)

Same `Guard`, same governed `http.Client` — only the provider client changes.
Point the OpenAI SDK's `WithHTTPClient` at the wrapped transport:

```go
import (
    "github.com/openai/openai-go"
    openaiopt "github.com/openai/openai-go/option"
)

hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
client := openai.NewClient(
    openaiopt.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
    openaiopt.WithHTTPClient(hc),
)

resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
    Model:    openai.ChatModelGPT4o,
    Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage("What's the weather in Paris?")},
})
// pre_llm governance + typed *flyedge.DenyError on err work identically. Gate any
// tool calls the model returns with guard.CheckToolCall(...) before executing them,
// exactly as in the Anthropic example above.
```

### Gemini (tool use)

Google's `genai` client accepts an `*http.Client` via `ClientConfig.HTTPClient`,
so the same wrap governs it:

```go
import "google.golang.org/genai"

hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
client, err := genai.NewClient(ctx, &genai.ClientConfig{
    APIKey:     os.Getenv("GEMINI_API_KEY"),
    HTTPClient: hc,
    Backend:    genai.BackendGeminiAPI,
})
if err != nil { /* ... */ }

resp, err := client.Models.GenerateContent(ctx, "gemini-3.6-flash",
    []*genai.Content{genai.NewContentFromText("What's the weather in Paris?", genai.RoleUser)}, nil)
// Same pre_llm governance and typed denials; gate any returned function calls
// with guard.CheckToolCall(...) before you execute them.
```

### Any LLM client with a pluggable HTTP client (e.g. langchaingo)

Any client that lets you set the `*http.Client` is governed the same way — wrap
the transport once, pass the client in. For `langchaingo`:

```go
guard, err := flyedge.New(flyedge.LoadEnv())
if err != nil { /* ... */ }
defer guard.Close()

hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}

// langchaingo's Anthropic provider — github.com/tmc/langchaingo/llms/anthropic,
// aliased here as lcanthropic to distinguish it from the raw Anthropic SDK —
// takes an *http.Client via WithHTTPClient.
llm, err := lcanthropic.New(lcanthropic.WithHTTPClient(hc))
if err != nil { /* ... */ }

reply, err := llms.GenerateFromSinglePrompt(ctx, llm, "What's the weather in Paris?")
// The call is now governed at the pre_llm stage.
```

The pattern is always the same: **one wrapped transport governs every model call
made through it.** You don't instrument each call site.

---

## Core Concepts

### The Guard

`*flyedge.Guard` is the handle for everything. Construct it once, near the start
of your program, and pass it where it's needed.

```go
guard, err := flyedge.New(cfg, opts...)
```

- `New` **validates** the config (identity, URL, mode), establishes the signer
  and enforcer, and **starts any owned background goroutines** (e.g. the cloud
  telemetry flusher). Nothing happens at import time — construction is the only
  side-effecting step, and it returns an error you can handle.
- `guard.Close()` flushes buffered telemetry and stops those goroutines. Always
  `defer guard.Close()`.

### The four stages

Flyedge governs an agent at four points in its loop. Each maps to an explicit
call in your code:

| Stage | What it guards | How you invoke it |
|---|---|---|
| `pre_llm` | The outgoing model request | `guard.WrapRoundTripper(base)` |
| `tool_call` | A tool the model wants to run | `guard.CheckToolCall(...)` |
| `post_llm` | The model's response text | `WrapRoundTripper(base, flyedge.WithResponseCheck())` or `guard.CheckModelResponse(...)` |
| `tool_call_response` | A tool's output before it re-enters context | `guard.CheckToolResponse(...)` |

You wire in only the stages you need. The `pre_llm` stage (wrapping the
transport) is the minimum useful configuration; adding `tool_call` checks is
what makes flyedge able to block dangerous actions.

### Decisions and denials

A check returns a `Decision` and an `error`:

```go
dec, err := guard.CheckToolCall(ctx, session, name, args, dest)
```

- On **allow**: `err == nil` and `dec.Action == "allow"`.
- On **warn**: the warning is advisory in `ModeWarn` and `ModeAudit`. In
  `ModeEnforce`, Flyedge upgrades it to a denial and returns `*flyedge.DenyError`.
- On **deny**: `err` is a `*flyedge.DenyError`. Use `flyedge.AsDenyError(err)` to
  recover it and read `.Reason`.
- On **kill switch** (the agent has been remotely halted): `err` is a
  `*flyedge.KillSwitchError`; use `flyedge.AsKillSwitchError(err)`.

Server-side **deny always enforces** in every checking mode. `ModeOff` is the
explicit exception because it skips the check entirely. Local controls have
their own posture (see [Protection Modes](#protection-modes)).

---

## Governing Model Calls

`WrapRoundTripper` wraps an `http.RoundTripper` so that requests through it are
checked before they leave the process.

```go
hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
```

By default it governs the **`pre_llm`** stage — the outgoing request (model,
prompt, system prompt hash). To also check the model's **response** (`post_llm`),
add `WithResponseCheck()`:

```go
hc := &http.Client{
    Transport: guard.WrapRoundTripper(http.DefaultTransport, flyedge.WithResponseCheck()),
}
```

Response checking is streaming-safe — it inspects the response without breaking
server-sent-event streaming from the model provider.

If you can't (or don't want to) route a response through the transport — for
example you already have the assembled text — check it directly:

```go
dec, err := guard.CheckModelResponse(ctx, session, model, responseText)
```

---

## Governing Tool Calls

Tool calls are the highest-leverage governance point: this is where an agent
reads a file, hits an API, or spends money. Check **before** you execute:

```go
dec, err := guard.CheckToolCall(ctx, session, toolName, argsJSON, destDomain)
if err != nil {
    if de, ok := flyedge.AsDenyError(err); ok {
        // Return the denial to the model as the tool result, so it can adapt.
        return toolResultBlock(tu.ID, "blocked by policy: "+de.Decision.Reason)
    }
    return err
}
// allowed (or warned) — run the tool
```

`destDomain` is the network destination the tool will contact (e.g.
`"api.stripe.com"`), which lets egress policy apply. Pass `""` if the tool makes
no outbound call.

After the tool runs, you can check its **output** before feeding it back to the
model — this catches exfiltration and prompt-injection in returned data:

```go
dec, err := guard.CheckToolResponse(ctx, session, toolName, toolOutput)
```

A common, robust pattern is to convert a denial into a tool *result* rather than
crashing the loop — the model sees "blocked by policy" and can choose a
different action.

---

## Selective Protection

You might look for a way to *filter* which tools or components are protected (a
`protect(only=[...])`-style config, per-tool flags, glob matches). The Go SDK has
**no such filter API** — and doesn't need one.

Protection in Go is applied **per call**, so scoping is simply *which calls you
guard*. To leave a tool ungoverned, don't wrap it in a `CheckToolCall`; to give a
high-risk tool stricter treatment, pass a distinct `destDomain` (or a
purpose-built session) so server-side policy can target it. There's no hidden
auto-instrumentation to opt out of — the governance boundary is exactly the set
of `WrapRoundTripper` transports and `Check*` calls you write.

```go
// Governed: the model call and the payment tool.
hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
// ...
if _, err := guard.CheckToolCall(ctx, session, "charge_card", args, "api.stripe.com"); err != nil {
    // handle deny
}

// Ungoverned by choice: a trivial local tool you don't check.
result := clock.Now() // no CheckToolCall — not part of the governance boundary
```

Per-tool *policy* still lives server-side (keyed by tool name and destination);
the client's job is only to decide what to submit for a decision.

---

## Sessions

A session ties related checks together for correlation and session-level risk
scoring. Attach a session ID to the context:

```go
ctx := flyedge.ContextWithSession(context.Background(), "user-42-conversation-9")
```

Any check made with that context (transport calls included) is tagged with the
session. Pass the session ID explicitly to `CheckToolCall` / `CheckToolResponse`
/ `CheckModelResponse` as well, so tool checks correlate with the model calls in
the same turn.

---

## Protection Modes

Two independent settings control behavior. Keep them straight — they answer
different questions.

### `Mode` — policy-check posture

```go
flyedge.ModeEnforce // block platform warn, deny, and kill decisions
flyedge.ModeWarn    // warnings advisory; deny and kill decisions block (DEFAULT)
flyedge.ModeAudit   // warnings advisory; deny and kill decisions still block
flyedge.ModeOff     // skip platform and local checks (local development)
```

`ModeOff` is the only mode that bypasses the policy call. In every other mode,
a platform denial or kill-switch decision blocks. Local controls use the mode
in their own `localcontrol.Config`, supplied by CompFly or configured in process.
They can add a local denial but cannot override one from the control plane.

### `FailMode` — what to do when the enforcement call itself fails

```go
flyedge.FailOpen   // allow the action if flyedge/prism is unreachable (DEFAULT)
flyedge.FailClosed // deny the action if flyedge/prism is unreachable
```

`FailOpen` favors availability (an agent keeps working if governance is down);
`FailClosed` favors safety (no action proceeds unchecked). Choose per your risk
posture:

```go
guard, err := flyedge.New(flyedge.LoadEnv(),
    flyedge.WithMode(flyedge.ModeEnforce),
    flyedge.WithFailMode(flyedge.FailClosed),
)
```

| Situation | `fail_open` | `fail_closed` |
|---|---|---|
| Prism reachable, policy = allow | proceed | proceed |
| Prism reachable, policy = deny | **blocked** | **blocked** |
| Prism unreachable | proceed | **blocked** |

---

## Agent Identity & CompFly Cloud

Flyedge identifies each agent with a **DID** backed by an Ed25519 keypair. The
DID is how the platform attributes telemetry and applies the correct policy to
*this* agent. You obtain a DID + key by registering the agent with CompFly (via
the platform UI, the API, or the MCP tools).

Provide the identity through config — the usual path is environment variables
read by `LoadEnv`:

```bash
export COMPFLY_AGENT_DID="did:compfly:..."
export COMPFLY_AGENT_PRIVATE_KEY_PATH="/path/to/agent-key.pem"
export COMPFLY_API_URL="https://prism.p.compfly.ai"   # default
```

```go
guard, err := flyedge.New(flyedge.LoadEnv())
```

Or wire the identity explicitly in code by supplying a signer:

```go
import "github.com/compfly-ai/flyedge-go/identity"

signer, err := identity.NewFileSignerFromPath("/path/to/agent-key.pem", "did:compfly:...")
if err != nil { /* ... */ }

guard, err := flyedge.New(cfg, flyedge.WithSigner(signer))
```

`guard.DID()` returns the active DID (useful for logging which agent published a
given event).

Requests to prism are signed with the agent's key, so the platform can verify
authenticity. In **proxy mode** (`Config.ProxyMode`), traffic is routed *through*
the gateway (prism's `/v1/proxy`) rather than checked out-of-band.

---

## Manifest Registration

`Connect` registers the agent's manifest with the platform: its framework, the
tools and models it uses, and its environment. This is what powers drift
detection (the platform can flag when the running agent diverges from its
declared manifest) and gives operators an inventory of what each agent is
allowed to do.

```go
err := guard.Connect(context.Background(), flyedge.ManifestInfo{
    Framework:   "anthropic-sdk-go",
    Tools:       []string{"get_weather", "search_web"},
    Models:      []string{"claude-sonnet-4-5"},
    Environment: "production",
})
```

Call `Connect` once at startup, after `New` and before serving traffic. It POSTs
to `/v1/flyedge/connect` with your signed identity.

---

## Telemetry & OpenTelemetry

Every check produces a telemetry event. Where those events go is a pluggable
choice.

### Built-in sinks

- **Recorder** — the default; keeps an in-memory protection summary and performs
  no I/O.
- **Noop** — discards events when no recording is wanted.
- **Batched (CompFly cloud)** — buffers events and flushes them to the CompFly
  platform on an interval. Enable with `WithCloudTelemetry`:

  ```go
  guard, err := flyedge.New(flyedge.LoadEnv(),
      flyedge.WithCloudTelemetry(10*time.Second),
  )
  ```

### OpenTelemetry sink

The `telemetry/otel` subpackage exports each check as a `flyedge.check` span
(stage, action, model, latency) to **your** observability stack — Datadog,
Honeycomb, an OTel collector, or stdout for local debugging. Your application
owns the `TracerProvider` and its shutdown; the core module stays free of OTel
dependencies.

```go
import (
    "github.com/compfly-ai/flyedge-go"
    feotel "github.com/compfly-ai/flyedge-go/telemetry/otel"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Stand up an OTel pipeline you own.
exp, _ := stdouttrace.New(stdouttrace.WithPrettyPrint())
tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
otel.SetTracerProvider(tp)
defer tp.Shutdown(context.Background())

guard, err := flyedge.New(flyedge.LoadEnv(),
    flyedge.WithTelemetry(feotel.New(nil)), // nil → uses the global TracerProvider
)
```

The CompFly-native path (`WithCloudTelemetry`) and the OTel path
(`WithTelemetry`) are independent — you can run either, both, or neither.

### The protection report

`guard.Report()` returns a `Summary` (an alias for `telemetry.Summary`) with
per-stage counts, actions taken, and errors. It's printable and useful at
shutdown or in tests:

```go
fmt.Println(guard.Report()) // e.g. "4 allowed, 1 denied, 0 errors"
```

---

## Error Handling

Denials and kill switches are typed errors. Recover them with the `As...`
helpers rather than string-matching:

```go
_, err := guard.CheckToolCall(ctx, session, name, args, dest)
switch {
case err == nil:
    // allowed or warned — proceed
case flyedge.AsKillSwitchError(err) != nil:
    // agent has been remotely halted — stop the whole loop
    log.Fatal("kill switch engaged")
default:
    if de, ok := flyedge.AsDenyError(err); ok {
        // policy denial for THIS action — skip it, report to the model
        return fmt.Sprintf("blocked by policy: %s", de.Decision.Reason)
    }
    // a genuine transport/other error — governed by FailMode
    return err
}
```

- `*flyedge.DenyError` — the action violated policy. `.Reason` explains why.
  Recover with `flyedge.AsDenyError(err) (*DenyError, bool)`.
- `*flyedge.KillSwitchError` — the agent has been halted platform-side; stop
  everything. Recover with `flyedge.AsKillSwitchError(err) (*KillSwitchError, bool)`.
- Any other error from a check is an operational failure (network, etc.) and is
  subject to your `FailMode`.

When `WrapRoundTripper` blocks a model call, the deny surfaces as the error
returned by your LLM client's call — check it there with `AsDenyError` too.

---

## Configuration

`flyedge.Config` is the single configuration struct. Build it by hand, or start
from `LoadEnv()` and override fields.

```go
type Config struct {
    APIURL     string        // prism base URL
    DID        string        // agent DID
    KeyPEMPath string        // path to Ed25519 private key (PEM)
    KeyPEM     []byte        // inline PEM (alternative to KeyPEMPath)
    Mode       Mode          // enforce / warn / audit / off
    FailMode   FailMode      // fail_open / fail_closed
    ProxyMode  bool          // route through the flyedge proxy
    Timeout    time.Duration // per-request timeout to prism
}
```

Defaults applied by `New` / `LoadEnv`:

| Field | Default |
|---|---|
| `APIURL` | `https://prism.p.compfly.ai` (`flyedge.DefaultAPIURL`) |
| `Mode` | `ModeWarn` |
| `FailMode` | `FailOpen` |
| `Timeout` | `30s` |

Construct-and-override:

```go
cfg := flyedge.LoadEnv()
cfg.Mode = flyedge.ModeEnforce
cfg.Timeout = 10 * time.Second
guard, err := flyedge.New(cfg)
```

Options passed to `New` take precedence over the config for the things they
cover (`WithMode`, `WithFailMode`, `WithSigner`, `WithEnforcer`,
`WithTelemetry`, `WithCloudTelemetry`).

---

## Environment Variables

`LoadEnv()` reads:

| Variable | Maps to | Notes |
|---|---|---|
| `COMPFLY_API_URL` | `Config.APIURL` | defaults to `DefaultAPIURL` |
| `COMPFLY_AGENT_DID` | `Config.DID` | the agent's DID |
| `COMPFLY_AGENT_PRIVATE_KEY_PATH` | `Config.KeyPEMPath` | path to the Ed25519 PEM |
| `COMPFLY_AGENT_PRIVATE_KEY` | `Config.KeyPEM` | inline PEM (alternative to the path) |
| `FLYEDGE_MODE` | `Config.Mode` | `enforce` / `warn` / `audit` / `off` |
| `FLYEDGE_FAIL_MODE` | `Config.FailMode` | `fail_open` / `fail_closed` |

Provide **either** `COMPFLY_AGENT_PRIVATE_KEY_PATH` (a file) **or**
`COMPFLY_AGENT_PRIVATE_KEY` (the PEM inline) — the inline form is handy in
containerized/CI environments where you inject the key as a secret.

---

## Graceful Shutdown

`guard.Close()` flushes any buffered telemetry (so you don't lose the last batch)
and stops the goroutines `New` started. Always defer it:

```go
guard, err := flyedge.New(flyedge.LoadEnv())
if err != nil { /* ... */ }
defer guard.Close()
```

In a long-running service, call `Close` on your shutdown path (after the HTTP
server has drained, before the process exits) so the final telemetry flush
completes.

---

## Complete Example

A self-contained Claude agent with model + tool-call governance, DID identity,
cloud telemetry, and a protection report. This is the runnable
`examples/docs-quickstart` (with the tool definitions filled in); for a fuller
multi-turn loop see `examples/reference-agent`.

```go
package main

import (
    "context"
    "fmt"
    "net/http"
    "os"
    "time"

    "github.com/anthropics/anthropic-sdk-go"
    anthropicopt "github.com/anthropics/anthropic-sdk-go/option"

    "github.com/compfly-ai/flyedge-go"
)

func main() {
    if err := run(); err != nil {
        fmt.Fprintln(os.Stderr, "error:", err)
        os.Exit(1)
    }
}

func run() error {
    // Guard from the environment; cloud telemetry every 10s.
    guard, err := flyedge.New(flyedge.LoadEnv(),
        flyedge.WithCloudTelemetry(10*time.Second),
    )
    if err != nil {
        return fmt.Errorf("flyedge init: %w", err)
    }
    defer guard.Close()

    ctx := context.Background()

    // Register the agent's manifest (framework, tools, models).
    if err := guard.Connect(ctx, flyedge.ManifestInfo{
        Framework:   "anthropic-sdk-go",
        Tools:       []string{"get_weather"},
        Models:      []string{"claude-sonnet-4-5"},
        Environment: "production",
    }); err != nil {
        return fmt.Errorf("connect: %w", err)
    }

    // One governed HTTP client for all model calls.
    hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
    client := anthropic.NewClient(
        anthropicopt.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
        anthropicopt.WithHTTPClient(hc),
    )

    const session = "reference-agent"
    sctx := flyedge.ContextWithSession(ctx, session)

    resp, err := client.Messages.New(sctx, anthropic.MessageNewParams{
        Model:     anthropic.ModelClaudeSonnet4_5,
        MaxTokens: 1024,
        Messages: []anthropic.MessageParam{
            anthropic.NewUserMessage(anthropic.NewTextBlock("What's the weather in Paris?")),
        },
        Tools: []anthropic.ToolUnionParam{ /* your tool definitions */ },
    })
    if err != nil {
        if de, ok := flyedge.AsDenyError(err); ok {
            fmt.Println("model call blocked:", de.Decision.Reason)
            return nil
        }
        return err
    }

    // Guard each tool call before executing it.
    for _, block := range resp.Content {
        tu := block.AsToolUse()
        if tu.Name == "" {
            continue
        }
        if _, err := guard.CheckToolCall(sctx, session, tu.Name, string(tu.Input), "api.weather.com"); err != nil {
            if ke, ok := flyedge.AsKillSwitchError(err); ok {
                return fmt.Errorf("kill switch: %s", ke.Error())
            }
            if de, ok := flyedge.AsDenyError(err); ok {
                fmt.Printf("tool %q denied: %s\n", tu.Name, de.Decision.Reason)
                continue
            }
            return err
        }
        // ... execute the tool, then optionally guard.CheckToolResponse(...) ...
    }

    fmt.Println(guard.Report())
    return nil
}
```

More runnable examples live under `flyedge-go/examples/`:

| Example | Shows |
|---|---|
| `docs-quickstart` | This guide's Complete Example, runnable |
| `agent` | Minimal governed agent (one wrap across Anthropic + OpenAI) |
| `openai` | Minimal governed OpenAI agent (model + tool call) |
| `gemini` | Minimal governed Gemini agent via `genai` (model + tool call) |
| `agentframework` | Microsoft Agent Framework Go with governed model and automatic tool calls |
| `reference-agent` | The complete integration surface: Connect + heartbeat, mode changes, local controls, all four stages, on-behalf-of identity, provider/model picker, chat REPL + HTTP serve mode, example custom control |
| `manual` | Direct `Check` / `CheckToolCall` calls without a client |
| `langchaingo` | Governing a `langchaingo` LLM via `WithHTTPClient` |
| `tools` | Tool-call + tool-response guarding |
| `otel` | OpenTelemetry span export |

---

## API Reference

### Construction

```go
func New(cfg Config, opts ...Option) (*Guard, error)
func LoadEnv() Config
```

### Options

```go
func WithSigner(s identity.Signer) Option
func WithEnforcer(e enforce.Enforcer) Option
func WithTelemetry(t telemetry.Telemetry) Option
func WithCloudTelemetry(interval time.Duration) Option
func WithMode(m Mode) Option
func WithFailMode(fm FailMode) Option
```

### Guard methods

```go
func (g *Guard) Check(ctx context.Context, req CheckRequest) (Decision, error)
func (g *Guard) CheckToolCall(ctx context.Context, session, toolName string, args any, destDomain string) (Decision, error)
func (g *Guard) CheckToolResponse(ctx context.Context, session, toolName string, result any) (Decision, error)
func (g *Guard) CheckModelResponse(ctx context.Context, session, model, text string) (Decision, error)
func (g *Guard) WrapRoundTripper(base http.RoundTripper, opts ...WrapOption) http.RoundTripper
func (g *Guard) Connect(ctx context.Context, info ManifestInfo) error
func (g *Guard) Report() Summary
func (g *Guard) DID() string
func (g *Guard) Close() error
```

### Wrap options

```go
func WithResponseCheck() WrapOption // also guard the post_llm stage (streaming-safe)
```

### Types

```go
type Mode string
const (
    ModeEnforce Mode = "enforce"
    ModeWarn    Mode = "warn" // default
    ModeAudit   Mode = "audit"
    ModeOff     Mode = "off"
)

type FailMode string
const (
    FailOpen   FailMode = "fail_open" // default
    FailClosed FailMode = "fail_closed"
)

const DefaultAPIURL = "https://prism.p.compfly.ai"

type Config struct {
    APIURL     string
    DID        string
    KeyPEMPath string
    KeyPEM     []byte
    Mode       Mode
    FailMode   FailMode
    ProxyMode  bool
    Timeout    time.Duration
}

type ManifestInfo struct {
    Framework   string
    Tools       []string
    Models      []string
    Environment string
}

type Summary = telemetry.Summary
```

### Context

```go
func ContextWithSession(ctx context.Context, id string) context.Context
```

### Errors

```go
type DenyError struct { Decision Decision } // read the reason via de.Decision.Reason
func (e *DenyError) Error() string
func AsDenyError(err error) (*DenyError, bool)

type KillSwitchError struct { Kills []KillInfo }
func (e *KillSwitchError) Error() string
func AsKillSwitchError(err error) (*KillSwitchError, bool)
```

### Subpackages

| Package | Purpose |
|---|---|
| `flyedge-go/identity` | Signers, DID handling (`NewFileSigner`, `NewFileSignerFromPath`, `Signer`) |
| `flyedge-go/enforce` | The `Enforcer` interface, `Decision`, `KillInfo`, and prism client |
| `flyedge-go/telemetry` | `Telemetry` interface + `Noop` / `Batched` / `Summary` |
| `flyedge-go/telemetry/otel` | OpenTelemetry sink (`feotel.New`) |

---

For platform concepts (policies, effective controls, the governance data plane)
that apply across all SDKs, see the CompFly platform docs.
