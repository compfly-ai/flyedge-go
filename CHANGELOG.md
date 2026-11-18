# Changelog

## Unreleased

- Send the turn's W3C `traceparent` on proxied model calls, so Prism records their pre- and post-LLM checks under the same turn as the SDK's own checks. A caller's own `traceparent` is kept when no turn trace is set.
- Report the SDK version from the module version the agent was built with, instead of a fixed `flyedge-go/0.8.0`. Builds without a released version report `flyedge-go/devel`.
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
