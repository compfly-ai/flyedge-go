# gemini — a governed Gemini agent

A minimal flyedge-protected agent on Google's **`genai`** SDK. The `genai` client accepts an
`*http.Client` via `ClientConfig.HTTPClient`, so the same `guard.WrapRoundTripper` wrap that governs
the Anthropic and OpenAI SDKs governs Gemini too — no provider-specific adapter. The `pre_llm` check
runs against prism before the model call; a policy Deny is a typed `*flyedge.DenyError`. It then
governs a tool call explicitly with `guard.CheckToolCall`.

## Run

```bash
export GEMINI_API_KEY=...
export COMPFLY_API_URL=http://localhost:8080      # your CompFly gateway (defaults to prod)
# Optional identity — without it, checks fail open (unenforced):
export COMPFLY_AGENT_DID=did:compfly:...
export COMPFLY_AGENT_PRIVATE_KEY_PATH=/path/to/agent.pem

go run ./examples/gemini
```

Env: `GEMINI_API_KEY` (required), `MODEL` (default `gemini-3.6-flash`), `PROMPT`, `FLYEDGE_MODE`
(`enforce`|`warn`). Same governed transport wrap as the `agent`, `openai`, and `langchaingo` examples.
