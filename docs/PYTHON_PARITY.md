# flyedge-go ↔ flyedge (Python) parity matrix

Status as of the spec-conformance pass (branch `fix/telemetry-session-correlation`).
This validates the plan's gate: *"write flyedge-go to the spec and validate it is
feature complete with the Python version."*

The dividing line is deliberate and comes from the multi-SDK strategy: the **wire /
protocol contract** must be identical across every SDK (it is what prism sees), while
the **framework-interception layer** (how each ecosystem auto-attaches to LangChain,
OpenAI, etc.) is irreducibly language-native and stays per-SDK. So "parity" means:
Go is conformant on everything prism can observe, and diverges only where the language
ecosystem forces it to.

## A. Wire / protocol contract — MUST be identical (prism-observable)

| Capability | Python | Go | Notes |
|---|---|---|---|
| Ed25519 prehash signing + `X-CompFly-*` headers | ✅ | ✅ | `identity/`, `enforce.PostSigned` |
| `POST /connect` manifest | ✅ | ✅ | `connect.go`; capability set aligned |
| `POST /check` — 4 stages (pre_llm/tool_call/tool_call_response/post_llm) | ✅ | ✅ | `stages.go`, `flyedge.go` |
| Kill switches (403 full-scope + `kills[]`) | ✅ | ✅ | `KilledError`, `Decision.Kills` |
| `GET /config` heartbeat poller | ✅ | ✅ | Phase A; owned goroutine |
| Dynamic `model_mode` (check/passthrough/gateway) | ✅ | ✅ | `guard.ModelMode()`, transport wrap |
| Session taints (`/taint`, `/taint/acknowledge`) | ✅ | ✅ | `taint.go` (this session) |
| Delegation / intent / task mandates | ✅ | ✅ | context helpers → `X-CompFly-*` headers |
| OBO / enterprise identity | ✅ | ✅ | `enforce/identity_context.go` |
| Simulation client (config-driven, WS telemetry) | ✅ | ✅ | Phase B (observe-mode proven) |
| Mode off/audit/warn/enforce | ✅ | ✅ | local posture; server deny/kill always wins |
| Telemetry: protection events | ✅ | ✅ | `telemetry/`, batched + OTel |
| Telemetry vocabulary: llm_io/tool_io/session_start/session_summary | ✅ (auto) | ✅ (explicit emit helpers) | `telemetry_emit.go` |
| Trace/span correlation (`traceparent`) | ✅ | ✅ | `trace.go` (this session) |
| Enrichment context (framework/layer/provider/origin_type) | ✅ | ✅ | `CheckRequest` wire fields |
| execution_context / auth_context | ✅ | ✅ | opt-in pointer structs |
| Operation.tool_args_json / mcp_server_id | ✅ | ✅ | `enforce/wire.go` |

**Contract verdict: full parity.** Every field prism deserializes has a Go path.
The only difference is ergonomic: Python auto-fills llm_io/tool_io/trace from its
framework hooks; Go asks the caller to emit them (explicit model). The bytes on the
wire are the same shape.

## B. Framework-interception layer — native by design (NOT ported to Go)

| Capability | Python | Go | Rationale |
|---|---|---|---|
| Auto-instrument LangChain/LlamaIndex/OpenAI/Anthropic | ✅ | ✗ | Go has no monkey-patch culture; callers use explicit `Check*` + `WrapRoundTripper` |
| OpenInference / OTel span *consumption* + classification | ✅ | ✗ | Python reads its own emitted spans; Go emits, doesn't re-ingest |
| Auto-printed protection summary at exit | ✅ | explicit `Report()` | gothonic: side effects are caller-invoked |
| Local PII/injection detectors | ✅ (optional) | ✗ (by design) | server-authoritative; see `pii-redaction-not-inline` |

These are **not gaps** — they are the language-native layer the multi-SDK strategy
keeps per-SDK. A LangChain user reaches for Python; a Go service uses explicit gates.

## C. Go-only surfaces (ahead of Python)

| Capability | Notes |
|---|---|
| `flyedged` daemon | flight recorder, coding-agent hook interception, endpoint scan — no Python equivalent |
| Export to the caller's own OTel pipeline | `telemetry/batched.go` OTel path |
| Explicit owned lifecycle (`Close()` stops all goroutines) | gothonic; Python relies on atexit |

## Conclusion

flyedge-go is **feature-complete against the Python SDK for the entire platform
contract**. Remaining differences are (1) the framework-attach layer, intentionally
native per the multi-SDK strategy, and (2) Go-only operational surfaces. This matrix
is the reference the Rust port (flyedge-rs) and the Rust-backed Python bindings must
also satisfy on section A — sections B/C stay language-native.
