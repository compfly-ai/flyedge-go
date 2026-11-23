# Changelog

## Unreleased

- Always send inline images and PDFs as `content.attachments`. Policy Enforcer decides from the agent's controls whether to inspect them and acknowledges a skip as `skipped_no_controls`. The per-agent `nontext_payload` setting and the `X-Agent-Payload-Revision` acknowledgment are removed, along with `WithAttachments` and `NontextPayloadAllowed`. A request with attachments is no longer retried text-only after a media failure; the failure follows FailMode. `WithoutAttachments()` builds a Guard that never sends attachments, for a host that inspects media through a separate Guard.

## 0.11.3

- Request usage in streamed OpenAI Chat Completions during admitted invocations, preserving explicit caller options and leaving calls outside invocations unchanged.
- Automatically recover dropped invocation sockets within the authorized lease on recovery-capable brokers, with bounded retries and no replay of interrupted work.
- Send the turn's W3C `traceparent` on proxied model calls, so Prism records their pre- and post-LLM checks under the same turn as the SDK's own checks. A caller's own `traceparent` is kept when no turn trace is set.
- Report the SDK version from the module version the agent was built with, instead of a fixed `flyedge-go/0.8.0`. Builds without a released version report `flyedge-go/devel`.

## 0.11.2

- Fix scenario mock completion: `required_matches: 0` permits an optional mock to be used once without marking evidence incomplete. Required occurrences and unexpected-call rejection are unchanged.
- Send reverse-invocation WebSocket Pings every 10 seconds instead of 30, while retaining the independent 30-second Pong timeout. This provides margin before a gateway idle timeout; it does not replay interrupted invocations.

## 0.10.0

- Recognize Google and Anthropic Vertex generation routes in `WrapRoundTripper`, including URL-derived model IDs, signed proxy forwarding, and publisher-specific response/SSE inspection.

## 0.9.0

- Add explicit canonical scenario tool mocks and separate requested, mocked and executed evidence, with scoped reverse-invocation completion snapshots. See [canonical scenario integration](docs/CANONICAL_SCENARIOS.md).
- Preserve scenario evidence when simulation telemetry credentials renew.

Optional inspection of text in images and PDFs sent by your agents. Enable it per agent to include supported attachments in policy checks. Inspection is disabled by default and may add latency.

- Reuses the existing agent configuration refresh and shared SDK policy boundaries.
- Keeps ordinary text checks independent and enforces explicit policy blocks.
- Includes inspection status and file/page context in audit records without storing attachment bytes.
- Reports the feature version in agent connection metadata.

Native validation covers Microsoft Agent Framework with OpenAI Chat Completions. Azure AI Foundry Agent Service/Responses paths require separate transport qualification and are not certified by that test.
