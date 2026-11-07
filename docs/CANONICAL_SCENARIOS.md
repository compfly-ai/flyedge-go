# Canonical scenario hooks

For a handler and tool example with history, concurrency and canonical mocks,
see the [scenario integration guide](SCENARIO_INTEGRATION.md).

The SDK accepts scoped canonical plans in simulation configuration under
`extra.scenario_overrides`. It emits ready and completion snapshots for reverse
invocations, correlated to the run, session and exact installed plan. A cancelled,
failed or panicking handler cannot report successful completion. Renewing the
telemetry token retains the current plan's evidence and occurrence counts.

## Instrument a tool

Applications must explicitly call `Guard.BeforeScenarioTool` after their normal
`CheckToolCall` policy gate and before executing the tool. The SDK does not replace
arbitrary application tool functions automatically.

1. Run `CheckToolCall` and honor its decision, including blocks and modified arguments.
2. Pass the effective arguments to `BeforeScenarioTool(ctx, session, tool, args, allowMock)`.
   `allowMock` must come from deployment configuration, never user input. Reverse
   invocations additionally require `InvocationOptions.AllowSimulationInjection`.
3. On an error, stop the call. On `mocked == true`, use the returned JSON value and
   skip the real tool. Otherwise execute the tool and call `RecordToolIO` (or
   `RecordToolIODetail`) with its actual result/error.
4. Apply `GovernToolResult` to either result and honor its decision before returning
   the result to the model. Mocking does not disable policy protection.

`CheckToolCall` records `component.requested`; the mock hook records
`component.mocked`; `RecordToolIO` records `component.executed`. Never record a mock
as a real execution. JSON null is a valid mocked result: use the separate `mocked`
boolean to determine whether the tool should execute.

Rules match a tool name, one-based occurrence and JSON Pointer argument predicates.
Only tools with enabled mock rules are isolated; other tools remain live. Unmatched
calls to an isolated tool fail unless the plan explicitly selects passthrough.
Ambiguous rules, evidence overflow or unavailable snapshot queueing prevent clean
completion. Reapplying an unchanged plan does not reset its evidence.

`ScenarioMockStatus` returns an owned evidence snapshot for diagnostics; it is not
an evaluation verdict. The runner must receive the matching completion snapshot
and evaluate the saved judges. The SDK supports before-tool return fixtures, not
argument replacement, automatic framework checkpoint restoration or semantic
grading. History fixtures are supported separately, as described below. State
input uses the existing invocation state contract; semantic grading belongs to
the platform runner.

Limits: 256 KiB plan, 64 rules, 1,024 evidence events and 4 MiB evidence per plan.

## Provide history fixtures

Set `InvocationOptions.AllowClientHistory` to true to accept caller-provided
history (the default is false). `InvocationRequest.History` is separate from live
`Messages` and `State`. The SDK validates and transports the fixture; the handler
must apply it to the agent's conversation without replacing its system policy.
A nil `History` means absent; a pointer to an empty slice means explicitly empty.
The handler owns applying that distinction to its conversation state.

History supports user/assistant messages, assistant
`toolCalls: [{id, name, arguments}]` with JSON object arguments, and `role: tool`
responses with `toolCallId` and text `content`. Every tool call must be resolved
before another conversational message. Supplied system roles are rejected.
Map these records to the framework's message types as prior conversation context;
do not execute the recorded tool calls again. This is history injection, not
automatic restoration of framework memory or checkpoints.

For langchaingo, the separate `github.com/compfly-ai/flyedge-go/langchaingo` module
does this mapping. `Messages(request)` returns `History` followed by `Messages` as
`[]llms.MessageContent` for `GenerateContent`. Pass the whole conversation, not only
the latest user message, or follow-up turns lose their context.

## Concurrent reverse invocations

`InvocationOptions.MaxConcurrentInvocations` defaults to 1 (zero selects that
same default) and accepts 1–64. Opt in only when the handler and its dependencies
support concurrent calls. The SDK starts bounded goroutines; developers do not
need to start a goroutine around the handler themselves. Calls sharing a session
cannot overlap. Cancellation targets one invocation and its slot remains occupied
until the handler returns; handlers must honor their context.

The connection advertises `scenario_execution_version: 2`. A `scenario_plan`
arriving on an authorized invocation is installed privately for its session;
mock counters and evidence are not stored in the agent-wide simulation plan.
Completion carries `scenario_snapshot` with `handler_success`. Mock permission
still requires `AllowSimulationInjection` and the application's tool hook opt-in.
Platform scheduling must negotiate this capability before sending inline plans.
