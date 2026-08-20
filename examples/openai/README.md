# openai — a governed OpenAI agent

A minimal flyedge-protected agent on the **OpenAI** SDK. It builds a `flyedge.Guard`, installs one
governed `http.Client` (`guard.WrapRoundTripper`) into `openai.NewClient` via `WithHTTPClient`, makes
a governed chat completion, and then governs a tool call explicitly with `guard.CheckToolCall`. The
`pre_llm` check runs against prism before the model call; a policy Deny is a typed `*flyedge.DenyError`
and the provider is never contacted.

## Run

```bash
export OPENAI_API_KEY=sk-...
export COMPFLY_API_URL=http://localhost:8080      # your CompFly gateway (defaults to prod)
# Optional identity — without it, checks fail open (unenforced):
export COMPFLY_AGENT_DID=did:compfly:...
export COMPFLY_AGENT_PRIVATE_KEY_PATH=/path/to/agent.pem

go run ./examples/openai
```

Env: `OPENAI_API_KEY` (required), `MODEL` (default `gpt-4o`), `PROMPT`, `FLYEDGE_MODE` (`enforce`|`warn`).
Same governed transport wrap as the `agent`, `gemini`, and `langchaingo` examples — only the provider
client changes.
