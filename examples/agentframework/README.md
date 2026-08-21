# agentframework — a governed Microsoft Agent Framework Go agent

This example integrates Flyedge with
[Microsoft Agent Framework for Go](https://github.com/microsoft/agent-framework-go).
It uses the framework's OpenAI Chat Completions agent and its automatic function-tool
calling loop.

Requires Go 1.25+, matching the current Microsoft Agent Framework Go module.

Flyedge is present at all of the relevant boundaries:

- The OpenAI client receives a governed `http.Client`, so every model request is checked before it
  reaches OpenAI.
- `governedTool` wraps the framework function tool, checking its arguments before execution and its
  result before the framework returns it to the model.
- `WithResponseCheck` checks a non-streaming model response before it is returned to the agent.

The explicit Chat Completions constructor is intentional. Microsoft Agent Framework's default
OpenAI agent currently uses the Responses API; this example uses the endpoint Flyedge's
provider-aware transport integration governs.

## Run

```bash
export OPENAI_API_KEY=sk-...
export COMPFLY_AGENT_DID=did:compfly:...
export COMPFLY_AGENT_PRIVATE_KEY_PATH=/path/to/agent.pem

go run ./examples/agentframework
```

Optional environment variables: `COMPFLY_API_URL`, `FLYEDGE_MODE`, `MODEL` (defaults to
`gpt-4o-mini`), and `PROMPT`.

For strict availability behavior, set `FLYEDGE_FAIL_MODE=fail_closed`; the SDK otherwise defaults
to `fail_open` when the gateway is unreachable.
