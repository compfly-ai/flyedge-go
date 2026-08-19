# flyedge-proxy example

`cmd/flyedge-proxy` is a standalone signing + policy-enforcing HTTP proxy. Any-language agent points
its LLM base URL at the proxy; the proxy runs a flyedge `pre_llm` check (via prism) before
forwarding to the real provider, and returns **403** on a policy Deny. Same core as the SDK wrap —
it's an `httputil.ReverseProxy` whose transport is `guard.WrapRoundTripper`.

## Run the proxy
```bash
COMPFLY_API_URL=https://prism.p.compfly.ai \
COMPFLY_AGENT_DID="$COMPFLY_AGENT_DID" \
COMPFLY_AGENT_PRIVATE_KEY_PATH=/path/to/agent.pem \
FLYEDGE_MODE=enforce \
go run ./cmd/flyedge-proxy            # listens on :9000
```

`COMPFLY_API_URL` points at your CompFly gateway (the SDK defaults to `https://prism.p.compfly.ai`
when unset). Register an agent and mint its DID + Ed25519 key in the CompFly platform, then set
`COMPFLY_AGENT_DID` and `COMPFLY_AGENT_PRIVATE_KEY_PATH`.

## Point an agent at it (no SDK change beyond base URL)
- **OpenAI SDK:** `option.WithBaseURL("http://localhost:9000/v1")` → `/v1/chat/completions` routes to OpenAI.
- **Anthropic SDK:** `option.WithBaseURL("http://localhost:9000")` → `/v1/messages` routes to Anthropic.
- **Any language / curl:** just send to the proxy; it forwards your provider API key unchanged.

## curl demo (Anthropic)
`demo.sh` sends an Anthropic Messages request through the proxy. The proxy checks it with prism,
then forwards to `api.anthropic.com` with your `x-api-key`. A benign prompt → Claude replies; a
policy Deny → `403 {"error":"policy_denied","reason":...}`.
```bash
ANTHROPIC_API_KEY=... bash examples/proxy/demo.sh "What are your store hours?"
```
