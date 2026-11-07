# Integrate a Go agent with canonical scenarios

Use this release branch with the matching Platform scenario runner. The handler
owns framework integration; the SDK transports validated history/state and installs
invocation-private mock plans. See [hook semantics](CANONICAL_SCENARIOS.md) and
[connection setup](../examples/remote-agent/README.md).

The fragments below assume an initialized, connected `guard`, a cancellable `ctx`,
and imports for `context`, `encoding/json`, and `github.com/compfly-ai/flyedge-go`.
`runAgent` and `readMessagesFromMailbox` are application functions, not SDK APIs.

## Register the handler

```go
allowInjection := true // Deployment setting, never supplied by a prompt.
opts := flyedge.InvocationOptions{
    Name: "My agent", Environment: "dev",
    Purposes: []flyedge.InvocationPurpose{flyedge.PurposePlayground, flyedge.PurposeSimulation, flyedge.PurposeEval},
    MaxConcurrentInvocations: 8, // Set to 1 (or omit) for sequential handlers.
    AllowClientHistory: true,
    AllowSimulationInjection: &allowInjection,
    // Add Contract with your state fields when consuming req.State.
}
handler := func(ctx context.Context, req flyedge.InvocationRequest, out flyedge.InvocationWriter) error {
    // Stateless composition. Preserve ToolCalls/ToolCallID during provider conversion.
    conversation := make([]flyedge.InvocationMessage, 0)
    if req.History != nil {
        conversation = append(conversation, (*req.History)...)
    }
    conversation = append(conversation, req.Messages...)
    tool := func(ctx context.Context, args map[string]any) (string, error) {
        return governedReadMessages(ctx, guard, req.SessionID, args, true)
    }
    // Keep your own system policy first. Apply state to your application's context.
    // The ordinary model loop must call tool when dispatching read_messages.
    reply, err := runAgent(ctx, conversation, req.State, tool)
    if err != nil { return err }
    return out.TextDelta(ctx, reply)
}
conn, err := guard.ServeInvocations(ctx, opts, handler)
if err != nil { return err }
defer conn.Close()
return conn.Wait()
```

Grant `PurposeSimulation` for Experiments (evals and simulations) and
`PurposePlayground` for Playground. Experiments look for a connection that grants
simulation; without it they fall back to the agent's HTTP endpoint, if one is
registered, or fail.

The handler's `ctx` carries the platform's session (`req.SessionID`). Pass that
`ctx` to your agent, or pass `req.SessionID` into your own session handling;
replacing it with an application-generated ID detaches the run's traces from the
experiment.

Concurrency accepts 1–64, using the same dispatch loop for every setting. Different
sessions can overlap; the same session cannot. The SDK rejects excess arrivals as
busy rather than queuing locally. Keep mutable conversation data local or keyed by
session. Shared clients must support concurrency; propagate `ctx` to external work.
This setting does not control parallel tools inside an invocation or add replicas.

## Instrument the tool

```go
func governedReadMessages(
    ctx context.Context, guard *flyedge.Guard, session string,
    args map[string]any, allowMock bool,
) (string, error) {
    const tool = "read_messages"
    argsJSON, err := json.Marshal(args)
    if err != nil { return "", err }
    decision, err := guard.CheckToolCall(ctx, session, tool, args)
    if err != nil { return "", err } // Includes policy denial.

    raw, mocked, err := guard.BeforeScenarioTool(ctx, session, tool, args, allowMock)
    if err != nil { return "", err } // Never fall through on a mock error.
    if !mocked {
        // Application function returning (json.RawMessage, error).
        raw, err = readMessagesFromMailbox(ctx, args)
        execution := flyedge.ToolIO{
            SessionID: session, RequestID: decision.RequestID, ToolName: tool,
            ArgsJSON: string(argsJSON), ResultJSON: string(raw),
        }
        if err != nil { execution.Err = err.Error() }
        guard.RecordToolIODetail(execution)
        if err != nil { return "", err }
    }
    // Both real and mocked results pass through response controls.
    result, _, err := guard.GovernToolResult(ctx, session, tool, string(raw))
    return result, err
}
```

`allowMock` comes from deployment configuration. Enabling it permits a matching
fixture; it does not force mocking. Check the `mocked` boolean because JSON null
is a valid fixture. Record only actual executions as `ToolIO`; the mock hook
records its own distinct evidence. Preserve normal model checks and model telemetry
in your model loop as well.

## History, state and schema boundaries

- History is opt-in, separate from live messages and state. It supports user and
  assistant messages, assistant `ToolCalls` with IDs/names/JSON-object arguments,
  and tool responses linked by `ToolCallID`. Convert them to provider message
  types without executing recorded calls. Supplied system roles are rejected.
- Nil history means absent; a pointer to an empty slice means explicitly empty.
  This stateless example treats either as an empty prefix. A stateful handler
  must initialize its conversation once: the runner supplies setup history again
  on each step, alongside the live conversation.
- Declare state through `InvocationOptions.Contract`. The handler applies it;
  the SDK does not restore framework checkpoints. Playground defaults are editor
  defaults, not runtime values automatically inserted into a scenario.
- The SDK installs `ScenarioPlan`; do not parse it in your handler. Current native
  execution supports before-tool return fixtures matched by tool name, occurrence
  and argument predicates. Nested model/subagent targeting and other schema
  override actions remain unsupported. Unrelated tools remain live.

## Verify your integration

Run a clean scenario, a mocked tool return, a supplied tool-history fixture, and
two concurrent scenarios with different mock values. Confirm that fixtures skip
real execution, history is not replayed, and outputs stay in their own sessions.
Disable injection permission and verify the mock scenario is rejected.

The [Rust, Python and TypeScript companion guide](https://github.com/compfly-ai/flyedge/blob/feature/atomic-scenario-pressure/docs/SCENARIO_INTEGRATION.md)
uses the same integration sequence. The basic Go remote-agent executable currently
demonstrates state and policy gates; it is not the complete history/mock example.
