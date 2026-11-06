# Changelog

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
