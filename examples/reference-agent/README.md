# Reference concierge: standard reverse invocation

A provider-backed agent demonstrating FlyEdge's normal integration path: connect
its manifest, open an outbound reverse-invocation connection, and run every turn
through the same governed model and tool loop. No inbound listener is required
for Playground or Experiments. OpenAI, Anthropic and Gemini are supported.

## Run

From `examples/`, configure your registered agent and one provider:

```sh
export COMPFLY_API_URL="https://<your-compfly-gateway>"
export COMPFLY_AGENT_DID="did:compfly:..."
export COMPFLY_AGENT_PRIVATE_KEY_PATH="/path/to/agent-key.pem"
export COMPFLY_ENVIRONMENT=dev
export OPENAI_API_KEY="<your-provider-key>"
go run ./reference-agent -provider openai -model "<your-model>"
```

Wait for `reverse invocation ready`, then open the registered agent in Playground
with the same environment. For local literal-loopback HTTP only, set
`COMPFLY_ALLOW_INSECURE_LOOPBACK=true`. The default is enforce/fail-closed; explicit
SDK posture environment settings override those defaults. Registration failure
stops startup. Provider validation checks the configured provider/model before
serving. Requests make real provider calls and can incur usage charges.

Ctrl-C/SIGTERM cancels the handler context and closes the connection and Guard.
Connection loss ends serving; supervise/restart the process without replaying
in-flight requests. SDK heartbeats and lease renewal are not application retries.

## State, history and mocks

Follow the [Playground walkthrough](../../docs/PLAYGROUND_SETUP.md). Enable starting
state and provide `{"location":"Paris"}`. This optional field is appended to the
current user turn as untrusted context. Unknown state fields and invalid locations
are rejected. The principal is chosen by local `-user alice` (default) or `-user bob`;
caller state cannot switch identity or supply policy.

The handler composes history followed by live messages for every invocation.
OpenAI, Anthropic and Gemini adapters preserve user/assistant messages, assistant
tool calls and their linked responses. Historical calls are never dispatched.
System policy is supplied by the application. This example is stateless between
invocations; the platform supplies the conversation. CLI turns remain independent.

Every tool checks policy, calls `BeforeScenarioTool`, then executes only if no
mock matched. Real executions record arguments/results separately; mocks retain
the SDK's own mock evidence. Both paths call `GovernToolResult`. A JSON null is a
valid mock. Use a canonical before-tool return fixture in an Experiment; do not
place mock plans in prompt text or state. Only matched tools are isolated.

Available tools are `get_profile`, `send_payment`, and `fetch_url` (see `agent.go`).
All three tool bodies are deterministic fixtures; even `fetch_url` does not make
a network request. The provider client does make real model requests.
Use dedicated test identities and deliberate mock coverage for simulation runs.
The example [web-fetch denylist](controls/web-fetch-denylist.yaml) illustrates a
local control; merely checking out this file does not publish a policy.

## Other entry points

```sh
go run ./reference-agent -cli          # explicit terminal chat
go run ./reference-agent -input 'Show my profile'  # one turn
go run ./reference-agent -serve        # optional HTTP compatibility interface
```

These modes do not also open a reverse connection. HTTP mode retains the existing
on-behalf-of header demonstration; it is a development fixture, not a production
authentication layer. Use reverse invocation for the standard Playground setup.

## Code map and tests

- `main.go`: configuration, manifest registration, signals and mode selection.
- `reverse.go`: state contract, serving options and session-preserving handler.
- `conversation.go`: provider-native history mapping and untrusted state context.
- `agent.go`: application identity and policy → mock/live → result gates.
- `providers.go`: provider clients on the governed transport and tool loops.

```sh
go test ./reference-agent ./remote-agent
```

These tests validate local wiring and conversation conversion; they do not prove
live deployment, provider acceptance or the policies configured for your agent.
