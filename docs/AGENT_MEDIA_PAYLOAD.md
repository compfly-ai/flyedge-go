# Agent image/PDF inspection

The Go SDK shares the existing Python/endpoint receiver contract:
`content.attachments` in the signed `/v1/flyedge/check` request. Prism and PE do
not require another decoding route for this sender.

The existing config heartbeat carries `agent_settings.nontext_payload` for the
authenticated agent. Only exact `allowed`, a resolved agent/namespace and a
signing identity enable inspection. Missing, null, skipped and invalid settings
disable it. The in-memory snapshot expires after three heartbeat intervals,
bounded to 15–900 seconds. There is no extra polling or org-level fallback.

`WrapRoundTripper` checks permission before extracting inline provider blocks.
Tool helpers use the same normalization and a separate text-only view. Disabling
inspection removes attachment bytes from policy checks while preserving the
original provider/tool input. Plain encoded user text keeps its existing text
route; it is not decoded as an attachment. URL/file references are unresolved;
the SDK never fetches a URL or reads an arbitrary local path to obtain bytes.

Normalization bounds recursion, visited nodes and transport size (16 MiB encoded
budget). Attachment bytes contribute to the content hash. The allow path requires
`nontext_status=decoded` or the explicit receiver oversize-skip acknowledgment.
Missing acknowledgment and technical media errors retry text-only once, then fail open
if unavailable; explicit control denials and kill switches still enforce. A proxy-mode media request first
uses the same SDK check, then follows its original route. No provider settings
are changed.

## Local customer-support agent

`examples/customer-support` reuses the Microsoft Agent Framework Go example with
OpenAI `gpt-4o-mini`, `lookup_order` and `lookup_return_policy`. The tools use
fictional records only and perform no writes or external fetches. Its chat page
accepts PNG/JPEG/PDF, up to 10 attachments and 10 MiB combined per turn. The local
demo also bounds retained uploads to 10 MiB per conversation, four in-memory
conversations and one concurrent reply. Start a new chat to release its history.
Non-streaming replies allow post-model denials to be withheld from the user.

The local Kubernetes manifest mounts the agent's own DID/key and references the
existing local OpenAI credential Secret. Credentials never reach the browser or
container build context. Local Tilt provides port 8096. Registration and live
local enforcement evidence are recorded separately; a successful compile or
SDK unit test is not end-to-end validation. Other Go frameworks/providers need
their own adapter validation before claiming coverage. The TypeScript counterpart is validated separately.


## Native local validation, 2026-10-20

Real model/tool calls, PNG, original one/three-page PDFs, earlier malformed-media handling,
permission off/on and 11-file/>10 MiB upload rejection passed. Existing scoped
PII/secret controls blocked public synthetic test images and a scanned PDF whose
identifiers appeared only on page three; a subsequent benign model/two-tool turn
passed. Persisted local audit records independently confirm Luna decoding,
attachment MIME/size, three PDF pages, page visual context, allow/deny and the
absence of media processing on disabled sessions.

Evidence: `/Users/prakaz/dev/validation/sdk-media-parity-20261020`. The local
browser upload picker still needs manual user verification because Chrome file
URL access is disabled. The pre-existing local audit bridge duplicates some
policy denial rows; one carries the media context. No platform source changes,
commits, PRs or cloud rollout were made for this Go validation.
