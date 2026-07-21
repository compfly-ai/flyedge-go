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

---

## 13. Simulation layer + config poller — design (Phase A + B)

**Status:** design — Phase 0 review gate (2026-07-20). **Behavioral source of truth:** the Python
`flyedge/simulation/` subsystem + prism `src/flyedge/` (`simulation_config.rs`, `config.rs`,
`simulation_telemetry.rs`, `handlers.rs`) + agent-eval (the driver).

### 13.0 Why
The simulation / attack-injection vertical is **built and wired everywhere except Go**: agent-eval
`PUT /internal/v1/agents/{id}/simulation` → prism → Redis `sim:config:{agent}` → SDK polls
`GET /v1/flyedge/config.simulation` → SDK middlewares + WS `/v1/simulation/telemetry` → prism →
Redis pub/sub `sim:telemetry:{runId}` → eval-runner (4-state outcomes). Today a **Go agent cannot be
a simulation/red-team target** — `flyedge-go` never polls `/config` and has no simulation client.
Closing that is the priority; it also lands the config poller that §7 already promised but never built.

### 13.1 Frozen wire additions (server already speaks these)
- **`GET /v1/flyedge/config`** → `FlyedgeConfigResponse` (fields omitted when null): `simulation?`,
  `model_mode? ∈ {check,passthrough,gateway}`, `manifest_refresh_required?`,
  `heartbeat_interval_seconds?`. Heartbeat headers on the request: `X-Agent-Heartbeat: 1`,
  `X-Agent-Manifest-Hash`, `X-Agent-Hostname`. Config is looked up by **agent slug**.
- **`simulation`** = `{active, run_id, middlewares[], telemetry_jwt, telemetry_url,
  protection_disabled, extra}`. `extra.attack_injector` carries `{mode, tier, profile, attack_config}`.
- **`GET /v1/simulation/telemetry`** (WS): `Authorization: Bearer <telemetry_jwt>` (HS256, run_id in
  claims, bounded to JWT TTL). Each text message is a `RuntimeEvent` JSON, republished to Redis
  `sim:telemetry:{run_id}`.
- **`RuntimeEvent`** (Python `simulation/types.py`): `event_id, run_id, prompt_id, timestamp,
  component_type, component_name` + optional `framework, llm_messages, llm_model, llm_response,
  llm_tool_calls, tool_name, tool_args, tool_result, tool_error, retriever_query, retriever_results,
  flags[], injection_id/strategy/target/sophistication/chain/tier, agent_profile`. Values truncated
  at 4096 chars; docs/tool_calls capped at 20. `prompt_id` correlates to prism's
  `X-Simulation-Prompt-Id` response header.

### 13.2 Phase A — config heartbeat poller (the dependency)
An **owned goroutine** (§7's "config poller", finally built), started when `Connect` succeeds
(`ConnectResponse` returns `heartbeat_interval_seconds`; default 5s, matching the Python client):

```go
// internal to Guard; no new required caller code.
func (g *Guard) startConfigPoll(interval time.Duration)  // owned; stopped by Close()
func (g *Guard) ModelMode() ModelMode                     // check|passthrough|gateway accessor
```
Each tick: signed `GET /config` (signature over `SHA-256(ts‖"")` — empty GET body) with the three
`X-Agent-*` headers → parse response → (a) hash the `simulation` sub-object; on change hand it to the
sim controller (§13.3); (b) on `model_mode` change fire `WithModeChangeHandler` + update
`ModelMode()`; (c) on `manifest_refresh_required` re-call `Connect` (default) or `WithManifestRefreshHandler`.
Options: `WithHeartbeat(interval)`, `WithModeChangeHandler(func(old,new ModelMode))`,
`WithManifestRefreshHandler(func())`, `WithSimulation(false)` to opt a build out entirely.

### 13.3 Phase B1 — simulation client core (observe-capable target)
The poller drives an internal `simController` — **simulation is automatic** for any agent that built a
`Guard` and installed the transport wrap; no extra caller code. Mirrors Python
`SimulationConfigHandler`:

- **Lifecycle** state machine `INACTIVE→STARTING→ACTIVE→STOPPING`. `on config change`: absent/invalid
  → deactivate; same `run_id` → hot-swap injector tier; changed `run_id` → restart. `Close()` forces
  deactivate + restore.
- **WS transport** (`simulation/ws.go`, mirrors `ws_transport.py`): dedicated goroutine, connect with
  `Authorization: Bearer <telemetry_jwt>`, bounded send queue (drop-on-overflow), batch drain,
  exp-backoff reconnect (1→30s) until stop. **Heartbeat cadence:** RuntimeEvent every 5s for the
  first 60s, then 30s keepalive — deliberately beats the eval-runner subscriber race.
- **`protection_disabled`**: while active, the Guard **short-circuits `Check*` to Allow** (skips the
  `/check` round-trip) so the agent's raw behavior is measured; restores real checking on deactivate.
  (Consistent with prism, which itself returns `allow`/`simulation_bypass` when protection_disabled —
  this is the one sanctioned suspension of "server deny always enforces".)
- **`behavior_monitor`** (`simulation/behavior.go`): observe-only flag detectors over
  content/args/result (`credential_exposure`, `external_url_in_tool_args`, `privilege_escalation_pattern`,
  `code_execution_pattern`, …), stamped onto `RuntimeEvent.flags`.
- **`telemetry`**: emit a `RuntimeEvent` per intercepted LLM/tool operation the Guard already sees.

### 13.4 Phase B2 — attack injector (the gothonic injection model)
The key design decision: **Go doesn't monkeypatch — it flips the interceptors it already owns from
observe to mutate.** The Guard already sits in the LLM-request path (`WrapRoundTripper`) and the
tool path (`CheckToolCall`/`CheckToolResponse`); in attack mode those same seams rewrite payloads.
Python strategy → Go interception point:

| Python strategy (`attack_schedule.py`) | Go injection point |
|---|---|
| `config_inject` (adversarial system message into LLM `messages`) | `WrapRoundTripper` **request rewrite** — the wrap already parses Anthropic/OpenAI bodies for pre_llm; in attack mode it *injects* a system message before signing/forwarding |
| `tool_poison` / `error_inject` (mutate tool result/error) | tool path — `CheckToolResponse` returns a **poisoned/replacement** result in attack mode |
| `rag_harvest` / `memory_poison` | **only** if the agent routes retrieval/memory through explicit Guard hooks (`InterceptRetrieval`/`InterceptMemory`) — no auto-injection, documented as opt-in |

This is **more bounded and honest than Python's pervasive patch**: only flows the caller actually
routes through the Guard can be injected (LLM messages always; tools if guarded; RAG/memory only via
explicit hooks). `ComponentProfiler` (`simulation/profiler.go`) builds the `agent_profile` from the
`Connect` manifest (tools/models) + observed operations and emits it in observe mode; payloads are
static templates keyed by strategy × sophistication L1–L4 (`attack_payloads.py`), placeholders
resolved from the profile. Tier transitions hot-swap via the poller's config-update path.

### 13.5 Packages + exported surface
```
flyedge-go/
  config_poll.go          // owned config heartbeat goroutine (Phase A)
  simulation/             // Phase B — internal controller, no core dep leakage
     controller.go        //   state machine, reacts to config.simulation
     ws.go                //   telemetry WebSocket transport (Bearer jwt, backoff, heartbeat burst)
     behavior.go          //   observe-only flag detectors
     injector.go          //   attack strategies mapped to Guard interception points
     profiler.go          //   ComponentProfiler → agent_profile
     types.go             //   RuntimeEvent + config structs (frozen wire)
```
New exported: `ModelMode` type + `Guard.ModelMode()`, `Guard.SimulationActive() bool`; options
`WithHeartbeat`, `WithModeChangeHandler`, `WithManifestRefreshHandler`, `WithSimulation(bool)`. WS is
the one new third-party dep (a small WebSocket client) — confined to `simulation/`, keeping core
stdlib-only. **No new required call** for basic participation: `New` + `Connect` + the transport wrap
is enough for a Go agent to become an observe-mode simulation target.

### 13.6 Milestones (this effort)
- **A** — config poller + `ModelMode()` + callbacks. Unit: hash-change fires once; manifest-refresh
  re-connects; signed GET accepted by prism.
- **B1** — sim controller + WS transport + protection_disabled + behavior_monitor + telemetry.
  E2E: a sim started against a Go agent yields observe-mode telemetry + agent_profile in eval-runner.
- **B2** — attack injector (config_inject + tool_poison + error_inject) + profiler + tier hot-swap.
  E2E: threat_hunt run injects and the 4-state correlator sees injections + what flyedge caught.

### 13.7 Local testing & verification
**Local status (audited 2026-07-20):** the simulation layer is fully present in the local stack's
*code* (prism, agent-eval + eval-worker, Redis all deploy under `just local`) but **not enabled** —
it's gated on env vars that the local manifests don't set. **No code changes to prism/agent-eval are
needed**; the gaps are purely env, in `terraform-compfly/local/`:

1. **prism** (`local/enforcement/prism.yaml` env) — currently missing, all required:
   - `SIMULATION_JWT_SECRET` — any non-empty dev value. Without it prism logs "Simulation disabled";
     `/config` never surfaces `simulation` and the telemetry WS returns 500 "Simulation not configured".
   - `INTERNAL_API_KEY` — dev value. Without it the entire `/internal/*` router (incl. the simulation
     PUT/DELETE) is not mounted → 404. Auth header is `x-internal-key`.
   - `SIMULATION_TELEMETRY_URL: ws://prism:8080/v1/simulation/telemetry` (in-cluster) or
     `ws://localhost:8080/...` (host client) — else it advertises the prod `wss://` URL.
2. **agent-eval** (`overlays/local/services/agent-eval.yaml`, `agent-eval-config` CM) — to *drive*
   sims: `PRISM_URL: http://prism:8080` + `PRISM_INTERNAL_API_KEY: <same value as prism
   INTERNAL_API_KEY>`. (Name mismatch is real: agent-eval reads `PRISM_INTERNAL_API_KEY`, prism reads
   `INTERNAL_API_KEY` — the *values* must match.)

**Prerequisite PR (Phase A companion):** a small `terraform-compfly/local` change adding the above
env. Prism is already `:8080` port-forwarded and Redis is up, so once the env lands both test paths work:

- **Full path** — trigger an eval in agent-eval; it PUTs prism's internal endpoint, the Go agent's
  poller picks up `simulation` from `/config`, streams over the WS, eval-runner correlates outcomes.
- **Hand-driven** (no agent-eval) — a host test `PUT http://localhost:8080/internal/v1/agents/{slug}/simulation`
  with `x-internal-key`, then run the Go agent and watch it poll `/config` + connect the WS.
  `agent-eval/examples/captain-whiskers/verify_simulation.py` is the working reference for the PUT+WS flow.

**Per-phase verification:** A — unit (hash-change fires once, manifest-refresh re-connects, signed GET
accepted) + hand-driven `/config` returns a seeded `simulation`. B1 — Go agent registered locally,
sim started, observe-mode telemetry + `agent_profile` land in eval-runner (Redis `sim:telemetry:{runId}`).
B2 — threat_hunt run: injections appear and the 4-state correlator distinguishes blocked vs compromised.
