# OpenAI Responses governance

`Guard.WrapRoundTripper` recognizes POST paths ending in `/responses`, including
`/v1/responses` and compatible Azure/gateway base paths, independently of model
name. Use the wrapped `http.Client` with the OpenAI client as usual; no model-name
routing or additional model-stage checks are required in the application.

A pre-model deny prevents provider I/O. A non-streaming post-model deny withholds
the output, including tool-only replies. Both checks use provider `openai` and
operation `responses`, retaining session, OBO/delegation and trace context through
the normal Guard path. Fail-open/closed and allow/warn/deny semantics are unchanged.
Request and response bytes are preserved unless an active simulation injects
instructions; request replay through `GetBody` retains the original outbound body.

Pre-model content includes instructions and string/typed input items, including
function results and call IDs. Post-model content includes typed output items,
including message text, refusals, function names and arguments. Encrypted reasoning
is omitted from inspection. File/image references are not fetched or interpreted.
Explicit `CheckToolCall` / `GovernToolResult` gates remain necessary at execution
boundaries, especially for tools executed while consuming streamed events.
Hosted tool execution is not governed by local function callbacks.

## State and limits

`previous_response_id`, `conversation` and input `item_reference` mark both checks
with `metadata.context_partial=true`. The SDK inspects the current request only;
it does not reconstruct provider-held context. Replay conversation items inline
when local inspection of prior content is required. This flag does not make opaque
reasoning or multimodal content readable.

Responses proxy/gateway mode forwards through Prism without duplicate SDK model
checks. This requires [Prism PR #316](https://github.com/compfly-ai/platform/pull/316)
to be deployed **before** upgrading proxy-mode clients: older Prism can forward
Responses without inspecting its content. Prism supports synchronous requests with
explicit input history; it rejects streaming, background generation and server-held
history references. SDK check mode retains stream auditing and partial-context support.

Coordinated SDK changes: [Go #31](https://github.com/compfly-ai/flyedge-go/pull/31)
and [Rust/Python/TypeScript #154](https://github.com/compfly-ai/flyedge/pull/154).
Background creation remains rejected by the SDK because retrieval is not governed.
WebSockets, retrieval and `/responses/compact` remain outside the supported generation
contract; other HTTP routes keep their existing pass-through behavior.

HTTP provider errors pass back without a successful post-model check. A 2xx failure,
queued/in-progress result, malformed JSON or missing output array returns an error.
Incomplete output is checked and marked `capture_incomplete`. Empty output still
records a post-model check. Simulation config injection prepends to `instructions`,
retaining existing guidance and input.

## Streaming (SDK check mode)

SSE bytes pass through unchanged. The SDK audits a bounded 1 MiB capture exactly
once at EOF/read failure or Close. `verdict_relay=stream_delivered` makes this an
audit of delivered output, not a blocking guarantee. Cancellation retains identity
for the final audit. `capture_truncated` marks the capture limit; missing, failed or
incomplete terminal events and early closure mark `capture_incomplete`.

The extractor handles text/refusal and function/custom-tool argument deltas plus
output items. Terminal response output is used when available to avoid duplicate
deltas; otherwise the delivered prefix is audited. Compressed SSE that reaches the
wrapper without decompression rejects explicitly. Applications must always close
response bodies and independently gate tool execution.

## Validation and rollout

`responses_test.go` uses fake provider transports and a recording enforcer to cover
pre/post denial, allow/warn, fail modes, byte preservation/replay, identity and session,
model-stage telemetry, multi-round tool results, partial context, unsupported modes,
provider failures, streaming cancellation/truncation and simulation instructions.
The existing provider transport suite covers Chat Completions, Anthropic and Gemini.

This change does not publish a release, update an agent SDK pin, remove its explicit
Responses workaround, or validate live OpenAI/Prism traffic. After pinning a released
fix and verifying native coverage, remove duplicate explicit model checks while
retaining the application's Responses loop and tool gates.

## Official OpenAI documentation

- [Responses API migration guide](https://developers.openai.com/api/docs/guides/migrate-to-responses): endpoint, request/output shapes and differences from Chat Completions.
- [Function calling](https://developers.openai.com/api/docs/guides/function-calling): function-call items, arguments, results and `call_id` correlation.
- [Streaming API responses](https://developers.openai.com/api/docs/guides/streaming-responses): typed server-sent events and stream handling.
- [Conversation state](https://developers.openai.com/api/docs/guides/conversation-state): manual history, `previous_response_id` and persistent conversations.
- [GPT-6.1 Sol](https://developers.openai.com/api/docs/models/gpt-6.1-sol): model-specific requirement to use Responses for tool calling.
