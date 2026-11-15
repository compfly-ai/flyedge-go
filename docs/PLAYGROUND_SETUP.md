# Connect an agent to Playground and simulations

Platform invocation is the standard integration for Playground and Experiments.
The agent opens an authenticated outbound WebSocket to CompFly and registers a
handler. You do not need a public agent HTTP endpoint, ingress rule or tunnel.
The application explicitly calls the serving API; constructing a Guard alone does
not start serving. Keep the connection alive for the lifetime of the process.

## 1. Register and configure the agent

Register a dedicated test agent in your CompFly deployment. Obtain its DID and
Ed25519 private key, and choose the agent environment (for example `dev`). Configure:

```sh
export COMPFLY_API_URL="https://<your-compfly-gateway>"
export COMPFLY_AGENT_DID="did:compfly:..."
export COMPFLY_AGENT_PRIVATE_KEY_PATH="/path/to/agent-key.pem"
```

Use real deployment values, not the placeholders. The URL selects the deployment;
the environment is a matching label within it. Store credentials outside source
control. For local development use a literal loopback URL and explicitly allow
insecure loopback in the invocation options. Hosted connections require HTTPS.

## 2. Connect and serve the ordinary governed agent

Create a Guard with explicit fail-closed behavior, connect its manifest (tools,
models, framework, environment), and register an invocation handler. Use the incoming
session ID for every model/tool check and observation, and pass cancellation to
application I/O. The handler must execute the same governed agent loop used by
other entry points.

Authorize `playground` and `simulation`; Experiments require `simulation` even
when running evaluations. Add `eval` for direct eval callers. Declare a state
contract, opt into client history, and permit scenario injection in deployment
configuration. Start with concurrency 1; increase it only when application
clients and session state are isolated. The SDK accepts 1–64 and prevents overlap
within one session.

Do not take the customer principal, policy, or permission to mock from supplied
state or prompt text. The example's application-selected principal stays fixed.

## 3. Open Playground and supply starting state

Start an example below and wait for its connection-ready message. In CompFly,
open the registered agent's Playground and select the matching environment. The
platform discovers its active invocation connection. A missing connection is a
registration/environment/purpose problem; do not work around it by silently
switching to an ungoverned handler.

For the support example, enable **Supply starting state** and set:

```json
{"selected_order_id":"DEMO-43"}
```

Ask “What is the status of my order?” The fixture should return `processing`,
subject to your configured policy. Switch to `DEMO-42` to get `shipped`.
These examples do not use an LLM or provider key. They demonstrate transport,
policy and scenario wiring; their answers do not measure model resilience.

The contract's `playground_default` / `PlaygroundDefault` values prefill the editor.
They are not injected when state is disabled, and they do not fill missing fields
in saved scenarios. Absent state and an explicitly empty object remain distinct.
The handler applies supplied state; the SDK does not restore framework memory.

## 4. Add a history fixture

Enable client history on the serving connection. A fixture can contain:

```json
[
  {"role":"user","content":"My order is DEMO-43."},
  {"role":"assistant","content":"I will use that order for this conversation."}
]
```

Supply this through the scenario's history setup, separately from its live user
messages and state, then ask “What do you remember?” The support examples return
prior user messages. In ordinary Playground chat, send the first turn and then the
follow-up in the same conversation. Saved scenario history is how to make that
starting conversation repeatable; typing JSON into the prompt does not install it.

For a real agent, convert `history` followed by live `messages` into the model's
message types. Preserve assistant `toolCalls` (`id`, `name`, object `arguments`)
and tool results linked by `toolCallId`. Never re-execute those recorded calls.
Supplied system-role messages are rejected; retain your application system policy.
The reference concierge's provider adapters show full tool-history conversion;
the deterministic support fixture only demonstrates user-message recall.

The examples compose conversations per invocation. An application that persists
its own history must seed supplied history once per session rather than append
the same setup on every turn. Empty history is explicit empty input; absent
history is no fixture. Define how your session store handles that distinction.

## 5. Run a simulation with a mocked tool

Save a scenario for the same agent/environment in Experiments and supply its
starting state and history. Configure a before-tool **return fixture**:

| Setting | Support-agent value |
| --- | --- |
| Tool | `lookup_order` |
| Occurrence | `1` (one-based) |
| Argument predicate | JSON Pointer `/order_id` equals `DEMO-43` |
| Returned JSON | `{"order_id":"DEMO-43","status":"delayed"}` |
| Required matches | `1` |

These are the rule's values, not a complete scenario-import document. Use the
canonical scenario authoring schema supported by your Platform version. Mocks
belong to the scenario setup; state and history are separate inputs. Playground
is useful for manual conversation/state checks; use Experiments for repeatable
mock plans and saved judges. Do not imply that setting a state field installs a mock.

The application tool path must be:

```text
policy check → before-tool hook → matched fixture OR real tool → result governance
```

If a fixture matches, skip the real tool and use the mock value, including JSON
null. If nothing matches and live execution is permitted, run the real tool and
record its actual arguments/result/error. Do not report mocked or historical calls
as real executions. A denied policy check or mock-hook error must stop the path;
it must never fall through to real execution. Unrelated unmocked tools remain live.

Only before-tool return fixtures are currently supported by these canonical
hooks. Do not represent them as arbitrary exception injection or automatic
checkpoint restoration. To simulate a provider failure, return an error-shaped
fixture that the application understands, or add an explicit application-owned
failure flag to its state contract and test that branch. Neither bypasses policy.

## 6. Verify behavior and shutdown

- Baseline: the real fixture runs; evidence includes `component.executed`.
- Mock: output uses `delayed`; evidence includes `component.mocked`, without a
  corresponding real execution for that call. Check required match counts.
- History: the follow-up sees prior context without executing historical tools.
- Isolation: two scenarios with different state/mocks do not share results.
- Denial: block the tool with a test policy and confirm it does not execute.
- Disabled injection: a scenario requiring a mock is rejected.

Keep the connection and Guard alive while testing. Close the connection before
closing the Guard on shutdown. Treat loss of the invocation connection as a service
failure; report it and let a supervisor restart the process. Do not replay
in-flight work automatically: the outcome may be unknown. Lease renewal keeps a
healthy connection authorized; it is not reconnection or permission to undo a
revocation. No SDK connection is created merely by importing the package.

## Run the Go reference agents

From `examples/`, with a registered identity and a reachable gateway:

```sh
# Deterministic order fixture; no provider key required:
go run ./remote-agent

# Real provider-backed concierge, standard outbound platform invocation:
export COMPFLY_ENVIRONMENT=dev
export OPENAI_API_KEY="<your-provider-key>"
go run ./reference-agent -provider openai -model "<your-model>"
```

The reference concierge uses optional `location` state, not `selected_order_id`:

```json
{"location":"Paris"}
```

It preserves typed history for OpenAI, Anthropic and Gemini, including linked
tool calls/results. Its fixed principal comes from the local `-user` option,
never from request state. Its tools are documented in the
[reference-agent guide](../examples/reference-agent/README.md). Select a tool
from that manifest when authoring its mock rules; `lookup_order` belongs only to
the deterministic support fixture above.

The standard reference-agent mode does not open an HTTP listener. `-cli` selects
terminal chat, `-input` runs a single turn, and `-serve` explicitly enables the
optional HTTP compatibility interface. These modes do not also start platform
serving. Use `COMPFLY_ALLOW_INSECURE_LOOPBACK=true` only for local loopback HTTP.

The reference agent calls `ServeInvocations` with these options:

```go
allowMocks := true
options := flyedge.InvocationOptions{
    Environment: "dev",
    Purposes: []flyedge.InvocationPurpose{
        flyedge.PurposePlayground, flyedge.PurposeSimulation, flyedge.PurposeEval,
    },
    AllowClientHistory: true,
    AllowSimulationInjection: &allowMocks,
    Contract: &flyedge.InvocationContract{State: flyedge.StateContract{
        Fields: map[string]flyedge.StateField{
            "location": {Type: flyedge.StateString,
                PlaygroundDefault: json.RawMessage(`"San Francisco, CA"`)},
        },
    }},
}
```

Imports: `encoding/json` and `github.com/compfly-ai/flyedge-go`. See
[reverse.go](../examples/reference-agent/reverse.go) for the handler and lifecycle,
[conversation.go](../examples/reference-agent/conversation.go) for native provider
history conversion, and [agent.go](../examples/reference-agent/agent.go) for the
policy → mock/live → governed-result tool path.

[Scenario integration](SCENARIO_INTEGRATION.md) and
[canonical hooks](CANONICAL_SCENARIOS.md) explain the SDK contract. For Rust,
Python or TypeScript use the [shared Rust SDK](https://github.com/compfly-ai/flyedge)
and its [Playground guide](https://github.com/compfly-ai/flyedge/blob/main/docs/PLAYGROUND_SETUP.md).
