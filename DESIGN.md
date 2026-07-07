# flyedge-go — Design

A ground-up Go implementation of the flyedge agent-protection SDK. Wire-compatible with the
existing prism/policy-enforcer pipeline; ergonomics redesigned to be idiomatic Go ("gothonic")
rather than a transliteration of the implicit Python SDK.

**Status:** design — for review before building. **Placement:** local scaffold (`flyedge-go/`), repo decided later.
**Source of truth for behavior:** the Python SDK (`flyedge/flyedge/`) + TS port (`flyedge/flyedge-js/`).

---

## 0. Philosophy: gothonic, not pythonic

The Python SDK protects an agent by being *implicit*: `protect()` monkeypatches LangChain/OpenAI/
Anthropic classes at import time, walks `sys.modules` to auto-instrument OTel, stashes a process-
global "active protection" singleton in a `ContextVar`, patches `httpx.Client.send` twice, and
turns a policy DENY into a *synthesized fake framework message* (sometimes an exception). It's
powerful and invisible — you call one function and your whole process is rewired.

Go can't monkeypatch and shouldn't fake it. The design principle: **every seam is visible in the
caller's code.** Concretely:

1. **Explicit interception, not import-time patching.** The caller wraps their transport/client
   (`flyedge.WrapRoundTripper`, `flyedge.WrapModel`) or mounts middleware. No global mutation, no
   dependence on import order.
2. **A passed handle, not an ambient singleton.** Construct `*flyedge.Guard` once (config + deps
   in), pass it (or a wrapped client) down. No `get_active_protection()`.
3. **Decisions & denials are values.** `Check` returns `(Decision, error)`; a block is a typed
   `*DenyError`, not an exception surfacing through a patched stack or a fake `AIMessage`.
4. **`context.Context` for deadlines/cancellation/trace/stage** — not `ContextVar` thread-locals.
5. **Interfaces at the boundaries** (`Signer`, `Enforcer`, `Telemetry`, `Interceptor`) — each
   swappable and unit-testable; the wire behavior lives behind them.
6. **Owned lifecycle.** `New(cfg) (*Guard, error)` starts what it needs; `guard.Close()` stops it.
   Background goroutines are explicit and owned — no fire-and-forget daemon threads.
7. **Config is a struct with sane zero-values + functional options** — not env reads scattered
   across modules. `LoadEnv()` is one explicit call that maps `COMPFLY_*` → `Config`.
8. **Compile-time deps.** Framework adapters are separate packages you import on purpose; no
   runtime `try/except ImportError` capability probing.

---

## 1. Frozen wire contract (MUST stay compatible)

These match prism + policy-enforcer + the TS port exactly. Non-negotiable
for v1 — a Go agent must work against the current stack unchanged.

### 1a. DID identity + request signing
- Key: Ed25519. `keyID = hex(SHA-256(SubjectPublicKeyInfo DER))[:32]` (32 hex chars).
- **Signing** (prehash, manual — NOT PureEdDSA):
  ```
  digest    = SHA-256( ascii(decimal(timestamp_ms)) || body )   // ts as ASCII string, then raw body bytes
  signature = base64( Ed25519.Sign(privKey, digest) )           // sign over the 32-byte digest
  ```
  Go: `h := sha256.New(); h.Write([]byte(strconv.FormatInt(tsMs,10))); h.Write(body); sig := ed25519.Sign(priv, h.Sum(nil))`.
  (Ed25519 over a 32-byte message is fine; the caveat is we sign the *digest*, not the body — matches Rust `verify(&digest, &sig)`.)
- **Headers**: `X-CompFly-Signature` (b64), `X-CompFly-Key-ID` (32-hex), `X-CompFly-Agent-DID`
  (`did:compfly:<org_short>:<fingerprint>`), `X-CompFly-Timestamp` (ms epoch as string).
- Replay window: 300_000 ms past / 30s future skew (verifier-side; we just send fresh ts).
- Tenant: `X-Tenant-ID = did.split(":")[2]`.

### 1b. Enforcement protocol
- Endpoints on `COMPFLY_API_URL` (default `https://prism.p.compfly.ai`):
  `/v1/flyedge/check`, `/v1/flyedge/connect`, `/v1/flyedge/telemetry`, `/v1/flyedge/config`; proxy
  mode posts to `/v1/proxy` with `X-Destination` = original URL + `X-CompFly-Stage`.
- `/check`: signed JSON POST (signature over the exact body bytes) → response
  `{decision, reason, policy_version, message, warnings, signals_present, signals_missing, request_id}`.
  `decision ∈ {allow, deny, block, warn}`; **`deny`/`block` always enforce** regardless of mode.
- **Confirmed required request fields** (verified against the live gateway, M1): top-level
  `request_id, session_id, timestamp_ms, stage, component_type, component_name, method_name`,
  `content{preview, full, hash, size_bytes}` (hash = sha256 hex of full — required), and
  `operation{type, tool_name, tool_args_hash, model_id, dest_domain}`. Missing any → HTTP 422
  (a schema error that occurs AFTER signature verification — so a 422 still proves the signature
  was accepted). The Go client fills the derived fields (`timestamp_ms`, `hash`, `size_bytes`,
  `preview`, component defaults) automatically.
- Stages: `pre_llm | tool_call | tool_call_response | post_llm` (wire strings — reuse verbatim).

### 1c. Modes
`enforce | warn | audit | off` (`FLYEDGE_MODE`, default `warn`) — governs *local* detectors only;
server `deny` always enforces. Separate transport `FailMode: fail_open (default) | fail_closed`.

Everything else — API shape, interception, config, telemetry batching — is ours to redesign.

---

## 2. Architecture: core library + proxy binary

```
             ┌────────────────────────────────────────────────┐
  in-process │  Go agent  →  flyedge.WrapModel / WrapRoundTripper │  (SDK: explicit wrap)
     use     │                        │                          │
             └────────────────────────┼──────────────────────────┘
                                       ▼
                         ┌─────────────────────────┐
                         │      flyedge (core)      │   New(cfg) → *Guard
                         │  Guard.Check(ctx, Req)   │   ── the one call everything routes through
                         │   Signer · Enforcer ·    │
                         │   Telemetry · Modes      │
                         └───────────┬─────────────┘
                                     │ signed /v1/flyedge/check  (+ /proxy in proxy mode)
                                     ▼
                              prism → policy-enforcer
                                     ▲
  any-language │  agent → HTTP →  ┌──┴───────────────┐
    use        │                 │  cmd/flyedge-proxy │  standalone proxy built on core
             (network hop)       │  (signs+enforces)  │
                                 └────────────────────┘
```

- **Core library** (`flyedge`): the signing + enforcement + telemetry engine and the `Guard`
  handle. In-process Go agents use it directly via explicit wrappers.
- **Proxy binary** (`cmd/flyedge-proxy`): a thin HTTP proxy built on the same core, for
  any-language agents (they point their LLM base-URL at it; it signs + enforces + forwards). This
  is the explicit, network-level equivalent of Python's httpx monkeypatch.

Both share one enforcement/signing core — no duplicated wire logic.

---

## 3. Packages

```
flyedge-go/
  flyedge.go              // package flyedge: Guard, New, Config, Check, Decision, DenyError
  options.go              // functional options (WithEnforcer, WithSigner, WithMode, WithTelemetry…)
  config.go               // Config struct + LoadEnv() (COMPFLY_* → Config, one explicit mapping)
  identity/               // Signer: Ed25519 DID load + sign_request (§1a). No global state.
     signer.go            //   Signer interface + FileSigner (PEM), the prehash signing
     did.go               //   DID parse, keyID derivation, headers
  enforce/                // Enforcer: the /v1/flyedge/* client (sign→POST→typed Decision)
     client.go            //   HTTPEnforcer; Check/Connect/Telemetry/Config; FailMode handling
     wire.go              //   request/response structs = the frozen /check schema
  intercept/              // explicit interception primitives (no monkeypatching)
     roundtripper.go      //   WrapRoundTripper(http.RoundTripper) — proxy-mode transport
     model.go             //   WrapModel adapters live in subpkgs to keep core dep-free
  telemetry/              // Telemetry interface; OTel + batched /telemetry impls; the Report()
  proxy/                  // reusable proxy handler (used by cmd/flyedge-proxy)
  adapters/               // OPT-IN framework adapters, each its own module/deps
     openai/  anthropic/  langchaingo/   // WrapClient(...) explicit integrations
  cmd/flyedge-proxy/      // main: standalone signing+enforcing HTTP proxy
  DESIGN.md
```
Core (`flyedge`, `identity`, `enforce`) depends only on stdlib + `golang.org/x/…`. Provider/
framework SDKs are only in `adapters/*` — imported on purpose, never probed at runtime.

---

## 4. Core interfaces (the seams)

```go
// Signer produces the X-CompFly-* headers for a request body. (identity.FileSigner is the default.)
type Signer interface {
    Sign(body []byte, ts time.Time) (Headers, error) // Headers = map[string]string of the 4 X-CompFly-*
    DID() string
    KeyID() string
}

// Enforcer is the policy decision point (prism /v1/flyedge/check). Swappable for tests/offline.
type Enforcer interface {
    Check(ctx context.Context, req CheckRequest) (Decision, error)
}

// Telemetry receives protection events; OTel + batched-cloud impls, or Noop.
type Telemetry interface {
    Record(ctx context.Context, ev Event)
    Report() Summary   // the explicit replacement for Python's stop()-time PROTECTION SUMMARY
    Close() error
}

// Interceptor wraps a caller-owned thing (transport, model client) to route calls through Guard.
// Concrete wrappers (WrapRoundTripper, adapters) implement this contract explicitly.
```

`Guard` composes them:
```go
type Guard struct { /* signer Signer; enforcer Enforcer; tel Telemetry; mode Mode; fail FailMode */ }
func New(cfg Config, opts ...Option) (*Guard, error)
func (g *Guard) Check(ctx context.Context, req CheckRequest) (Decision, error) // sign → enforce → telemetry
func (g *Guard) Close() error
```

---

## 5. Interception model (explicit)

Three explicit entry points, in increasing invasiveness — the caller picks:

1. **Manual check** (most explicit): `dec, err := guard.Check(ctx, req)` around your own call.
   Nothing hidden; ideal for backends and custom loops.
2. **Transport wrap** (proxy mode): `httpClient.Transport = guard.WrapRoundTripper(base)`.
   Outbound LLM-provider requests get signed + routed to prism `/v1/proxy` with `X-Destination`.
   This is the explicit analog of Python's `httpx.Client.send` patch — but scoped to the client you
   choose, visible at the call site.
3. **Framework adapter** — RECONSIDERED (2026-07-06). Adapters are NOT needed for HTTP-based SDKs:
   the transport wrap (#2) already governs the Anthropic + OpenAI SDKs (and raw net/http) with one
   provider-agnostic implementation, proven by deleting the Anthropic adapter with no loss. Keep an
   adapter only when a framework needs semantics the HTTP layer can't give — in-process (non-HTTP)
   tool calls, per-call stage detection (tool_call vs pre_llm), or streaming hooks.

`context.Context` carries the stage (`flyedge.WithStage(ctx, StageToolCall)`) instead of Python's
`_current_stage` ContextVar — but it's a normal, documented ctx value the caller sets.

---

## 6. Decisions & denials as values

```go
type Decision struct {
    Action        Action   // Allow | Deny | Warn  (Block folds into Deny)
    Reason        string
    Message       string
    Warnings      []string
    PolicyVersion string
    RequestID     string
    Signals       Signals  // present/missing
}

// A block is a typed error the caller handles — never a fake AIMessage, never a panic.
type DenyError struct { Decision Decision }
func (e *DenyError) Error() string { return "flyedge: denied: " + e.Decision.Reason }
```
`guard.Check` returns `(Decision, error)`: `Deny`/`Block` → `Decision{Action:Deny}` **and** a
`*DenyError` (so callers can `errors.As` or inspect `Decision`). `Warn` → `Decision{Action:Warn}`,
nil error (caller decides). Transport failure honors `FailMode`: fail_open → `Allow`+nil;
fail_closed → `Deny`+`*DenyError{Reason:"enforcement_unavailable"}`. Server `deny` always enforces
regardless of `Mode`.

The proxy binary maps `*DenyError` → HTTP 403 with the reason; adapters return it up the call chain.

---

## 7. Config & lifecycle

```go
type Config struct {
    APIURL     string        // default https://prism.p.compfly.ai
    DID        string        // COMPFLY_AGENT_DID
    KeyPEMPath string        // COMPFLY_AGENT_PRIVATE_KEY_PATH (or KeyPEM inline)
    KeyPEM     []byte
    Mode       Mode          // default Warn
    FailMode   FailMode      // default FailOpen
    ProxyMode  bool
    Timeout    time.Duration // default 30s
    // no scattered getenv — LoadEnv() fills this once
}
func LoadEnv() Config                 // explicit COMPFLY_*/FLYEDGE_* → Config
func New(cfg Config, opts ...Option)  // validates; nil signer only if no key (check-only/offline)
```
`New` returns an error on bad config (not a silent fail-open). It starts the telemetry batcher +
config poller as **owned goroutines**; `Close()` stops them and flushes `Report()`. No import-time
side effects — importing `flyedge` does nothing until `New`.

---

## 8. The proxy binary (`cmd/flyedge-proxy`)

A standalone HTTP server built on the core `Guard`. Any-language agent sets its LLM base URL (or
`HTTPS_PROXY`) to it. Per request: read body → `guard.Check` (or proxy-mode sign+forward) →
on Allow forward to `X-Destination`/origin, on Deny return 403 + reason. Config via the same
`Config`/env. This gives language-agnostic reach without any in-process magic — the explicit,
deployable counterpart to the SDK.

---

## 9. Implicit (Python) → explicit (Go) mapping

| Python (implicit) | Go (explicit) |
|---|---|
| `setattr(cls, m, patched)` class monkeypatch | caller wraps their client/transport (`WrapRoundTripper`, adapters) |
| `auto_instrument()` walking `sys.modules` | opt-in `telemetry.NewOTel()` the caller wires |
| double `httpx.Client.send` patch | one `WrapRoundTripper` on the caller's `http.Client` |
| import-time `_patches.install()` | plain constructors; composition via options |
| `_current_protection` global singleton | passed `*Guard` handle |
| `_scoped_config` / `_current_stage` ContextVars | documented `context.Context` values |
| DENY → fake `AIMessage` / sometimes raise | typed `Decision` + `*DenyError` value |
| framework via `type().__module__` substring | typed adapter packages, imported on purpose |
| optional-dep `try/except ImportError` | compile-time deps in `adapters/*` |
| fail-open on any exception | explicit `FailMode` enum |
| fire-and-forget daemon threads | owned goroutines + `Close()` |

---

## 10. Telemetry
`Telemetry` interface with three impls: `Noop` (default if unconfigured), `Batched` (background
goroutine → `/v1/flyedge/telemetry`, owned + flushed on `Close`), and `OTel` (opt-in gen_ai spans).
`Report()` returns a `Summary` struct (checks, allows, denies, warns, per-stage timings) — the
caller decides whether/how to print it (the explicit replacement for Python's auto-printed summary).

---

## 11. Milestones
1. **Core signing + enforcement (walking skeleton).** ✅ **DONE + live-verified (2026-07-06).**
   `identity.FileSigner` (wire-compatible sign), `enforce.HTTPEnforcer`, `Guard.New/Check/Close`,
   typed `Decision`/`DenyError`, `Config`/`LoadEnv`, functional options with injectable
   Signer/Enforcer. Unit tests: signer self-verifies over SHA-256(ts‖body) exactly as prism does;
   Guard Allow/Deny→DenyError/Warn + fail-open/closed via a stub enforcer. **Live:** a Go Guard
   built from the MCP-minted DID+key signed a `/v1/flyedge/check` request that **prism accepted**
   (`action=allow, policy_version=…`) — Go signing confirmed wire-compatible.
2. **Transport wrap (provider-agnostic).** ✅ **DONE + live E2E (2026-07-06).** `Guard.WrapRoundTripper(base)`
   — one `http.RoundTripper` that runs a pre_llm check before any LLM-provider request and forwards
   on Allow, denies via `*DenyError` (reachable through net/http's `*url.Error` via errors.As).
   Provider registry extracts prompt+model from **both** Anthropic (`/v1/messages`) and OpenAI
   (`/v1/chat/completions`) bodies. Installed identically into both SDKs via `option.WithHTTPClient`.
   Unit tests: one wrap governs both providers, deny blocks + unwraps, unknown hosts pass through.
   Live: the reference agent governs Claude through the wrap (benign → Claude replied).
   - **This RETIRED the per-framework adapter.** The `feanthropic` middleware adapter was deleted —
     the generic transport wrap subsumes it for any HTTP LLM SDK. Adapters return only if a future
     framework needs semantics the HTTP layer can't give (in-process tools, streaming stages).
   - Deferred within M2: `/v1/proxy` proxy-mode routing (needs gateway proxy seeding) and richer
     stage-in-ctx (tool_call/post_llm) — the pre_llm check is the proven core.
3. **Proxy binary.** ✅ **DONE + live E2E (2026-07-06).** `cmd/flyedge-proxy` = `httputil.ReverseProxy`
   whose Transport is `guard.WrapRoundTripper` (all extraction/check/deny reused). Routes by path
   (`/v1/chat/completions`→openai, `/v1/messages`→anthropic), forwards the agent's own provider key,
   maps `*DenyError`→403, `/health`. `session.go` adds `ContextWithSession` (proxy sets per-request
   session from `X-Session-Id`; wrap falls back to its own for a single SDK client). `examples/proxy`
   (README + curl `demo.sh`). Verified: a curl request → proxy → prism pre_llm check → allow →
   forwarded to Anthropic → Claude replied (any-language reach).
4. **Telemetry + Report.** ✅ **DONE (2026-07-06).** `telemetry` pkg: Telemetry interface
   (Record/Report/Close), Event, Summary, thread-safe in-memory Recorder (default) + Noop; Guard
   records each Check (stage/action/reason/model/latency/err), `Guard.Report()` returns the Summary,
   Close flushes. `WithTelemetry` injects a cloud/OTel sink later. examples/agent prints Report
   explicitly. (Cloud batcher + OTel bridge deferred as opt-in sinks.)

### Post-M4: stage refinements + range examples (2026-07-06)
- **Stage helpers** (`stages.go`): `CheckToolCall` / `CheckToolResponse` / `CheckModelResponse` —
  one-line gates for tool_call / tool_call_response / post_llm (pre_llm stays automatic via the wrap).
- **examples/tools**: Anthropic tool-use agent gating each tool at tool_call. **Live agent-level
  DENY** — Claude's `fetch_url(example.com)` was denied (`external_service_denied`) and never ran.
- **examples/langchaingo**: the same wrap governs a third framework (langchaingo) with no
  framework-specific code — range confirmed across raw Anthropic SDK, raw OpenAI SDK, and langchaingo.

### post_llm + streaming (2026-07-06, branch feat/flyedge-go-postllm)
- `WrapRoundTripper(base, WithResponseCheck())` adds response-side (post_llm) inspection.
  **Non-streaming responses are BLOCKED** on a post_llm deny (buffer → extract completion →
  CheckModelResponse → return *DenyError, drop the response). **Streaming (SSE) responses are
  MONITORED**: a streamMonitor tees bytes to the caller unchanged and runs one post_llm check on
  the accumulated completion at stream close (record/audit) — already-sent tokens can't be
  retracted, so streaming is honestly monitor-not-block. Response extractors + SSE delta parsing
  for both Anthropic and OpenAI. Verified live: agent with both stages → "2 checks — 2 allowed"
  (pre_llm request + post_llm response); unit tests cover non-stream block + stream monitor.
5. **Anthropic SDK adapter + reference agent.** ✅ **DONE + live E2E (2026-07-06, pulled ahead of M2-4).**
   `adapters/anthropic` — `feanthropic.Guard(g)` returns an `option.RequestOption` (SDK-native
   middleware) that runs a pre_llm flyedge check on each Messages request; extracts system+messages
   text, sets a session/request id, denies via `*DenyError` (no Anthropic call). `examples/agent` —
   a minimal Claude agent that builds the guard from the DID identity + prism and calls
   `client.Messages.New`. Verified E2E against the live stack + Anthropic: benign prompt → pre_llm
   ALLOW → **Claude replied**; a server-side deny → `*DenyError` → "BLOCKED by policy". Env-var-free
   at call sites — the guard is installed explicitly at client construction.
   - **Wire note (found live):** an empty `session_id` on `/check` hits a PE eval path referencing
     `radar.security.stateful_risk` (absent under the trimmed local detector set) → `evaluation_error`.
     The adapter always sets session + request ids (correct anyway for multi-turn) → clean allow.

Each milestone is independently runnable against the local stack (the auth/LEAD/detector tiers).

## 12. Decisions & open questions
**Decided (2026-07-06):**
- **SDK-first**: build the core + in-process SDK (M1→M2) before the proxy binary (M3).
- **First adapter = Anthropic Go SDK** (`github.com/anthropics/anthropic-sdk-go`), plus a small
  reference agent built on it — mirrors the local flyedge-demo (Claude), so we can drive it against
  the running stack end-to-end.

**Still open:**
- **Signing prehash**: confirm prism verifies `Ed25519.Sign(SHA-256(ts||body))` (prehash) vs
  PureEdDSA over the same bytes — the map says prehash; the M1 test nails it against the live verifier.
- Module path / repo (deferred): `github.com/compfly-ai/flyedge-go`? or `flyedge/go` subtree?
