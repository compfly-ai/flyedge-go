# Canonical scenario hooks

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
argument replacement, history/checkpoint restoration or semantic grading. State
input uses the existing invocation state contract; semantic grading belongs to
the platform runner.

Limits: 256 KiB plan, 64 rules, 1,024 evidence events and 4 MiB evidence per plan.
