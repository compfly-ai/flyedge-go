# Remote support agent

An explicit, outbound-only FlyEdge connection lets Playground and simulations invoke
this handler without a public listener or tunnel. This deterministic example uses
one read-only `lookup_order` fixture and real SDK policy checks and telemetry.
It does not use a model provider; evaluation scores measure the fixture, not model resilience.

The default terminal log is compact and color-coded: input in cyan, tool activity
in purple, output/completion in green, and failures in red. JSON tool results are
indented. Invocation IDs are shortened; session and run information appears once
at the start, instead of repeating the signed session value on every line.

- `-color auto` (default) enables colors on a terminal; `NO_COLOR` disables automatic color.
- `-color always` forces color; `-color never` disables it.
- `-log-format text` or `-log-format json` preserves full correlation fields for log collection.

Full input and output content is logged, including simulation-injected content;
use test data. Terminal control characters are escaped. Restart the example to
load code changes.

## Lease renewal

Calling `ServeInvocations` is the explicit opt-in. The default lease duration is
one hour and renewal is enabled. The SDK renews the same connection halfway
through each lease, so a healthy connection keeps running until you cancel its
context, close the connection/Guard, or Platform revokes it. Renewal does not
restart handlers or create observability sessions: invocation/session/run IDs
continue to identify individual work independently of the connection.

```go
options := flyedge.InvocationOptions{
    Environment: "dev",
    Purposes: []flyedge.InvocationPurpose{flyedge.PurposePlayground},
    // LeaseDuration: time.Hour, // default; range 1s–24h
    // AutoRenew: nil,           // default: true
}
```

For a finite session, set `AutoRenew` to a pointer to `false`. In this example,
use `-lease-duration 30m -auto-renew=false`. `-lease-duration 10s` is useful for
local renewal testing. `connection.ExpiresAt()` returns the latest acknowledged
lease expiry. The UI labels it as the current lease expiry, not a shutdown time,
when automatic renewal is enabled.

Renewal requires broker acknowledgement and cannot recreate a revoked/expired
lease. A lost connection or a lease that cannot be renewed still ends serving;
there is no automatic reconnect or invocation replay in this version. Changing
flags/config requires restarting the example; configuration is not hot-reloaded.

Register a dedicated agent and generate its DID identity through Platform. Set:

```sh
export COMPFLY_API_URL=https://your-prism
export COMPFLY_AGENT_DID=did:compfly:...
export COMPFLY_AGENT_PRIVATE_KEY_PATH=/private/path/agent.pem
cd examples
go run ./remote-agent
```

For the local Tilt stack, use `http://127.0.0.1:8080`, set
`COMPFLY_SIM_TELEMETRY_URL=ws://127.0.0.1:8080/v1/simulation/telemetry` if needed.
This dedicated example explicitly enables simulation hooks and literal-loopback
HTTP in its code. Reverse invocation serving remains explicit. Client state is
enabled by default; set `InvocationOptions.AllowClientState` to a pointer to
`false` to disable it. Declaring a state contract requires client state to be enabled.
The example uses the `dev` agent label in code. `COMPFLY_API_URL` selects the
local or hosted deployment.
Unprotected baseline runs are rejected. Use shielded simulation runs.

Platform discovers the lease by authenticated organization, agent slug, environment
and purpose. The existing persisted HTTP endpoint is not changed. The agent must
already exist in the evaluation catalog; this API does not create identities.

V1 supports one serving connection per agent/environment and one invocation at a time.
SDK output streams over the socket; the Platform adapter buffers the final response.
Disconnects are terminal with potentially unknown in-flight outcomes: no automatic
reconnect, retry or replay. Restart explicitly to authorize a new session. Platform
can revoke the lease; closing the Guard also closes the connection. Cancellation is
cooperative and cannot undo side effects. Acting-principal delegation is not supported.

The caller supplies the handler to `Guard.ServeInvocations`; importing the SDK,
constructing a Guard, and normal `Connect` never expose an invocation handler.
TLS is required except for an explicitly allowed literal loopback HTTP address.
Registration uses the agent's Ed25519 signing identity and fresh nonce; the socket
uses a single-use, expiring lease token. No additional application encryption is implemented.

## Local validation (2026-10-26)

The local Tilt integration used `remote-sdk-support-20261026` with an endpoint-free
snapshot `snap_20261026_a48b36ac`. Run `run_20261026_fdb29f25d5b8` completed with
21 scenarios / 35 messages, zero execution errors, and 154 received runtime events;
`simulationStatus` was `active`. The agent and judge were deterministic fixtures.
The preceding run exposed a Go prompt/session correlation mismatch; the SDK now
preserves the full session ID and emits the prompt portion for evaluation correlation.
Unsigned registration, cross-tenant dispatch, wrong-environment dispatch and
post-revocation dispatch were rejected in the local stack. Platform revocation
terminated the SDK serving session. The agent was stopped after validation.

For the existing local fixture on this workstation:

```sh
export COMPFLY_API_URL=http://127.0.0.1:8080
export COMPFLY_AGENT_DID="$(cat /tmp/compfly-remote-invocation/agent.did)"
export COMPFLY_AGENT_PRIVATE_KEY_PATH=/tmp/compfly-remote-invocation/agent.pem
export COMPFLY_SIM_TELEMETRY_URL=ws://127.0.0.1:8080/v1/simulation/telemetry
go run ./remote-agent
```

Run from the `examples` directory with the local stack up. Ctrl+C stops serving.
The temporary identity files are not portable or committed; generate a new identity
through Platform when recreating the fixture on another machine.


### Scenario-owned client state

The application receives optional logical values in `InvocationRequest.State`
as `map[string]json.RawMessage`. Decode them into domain objects, protobuf messages,
or mock application/session context before invoking your agent. Nil means absent;
a non-nil empty map means explicitly empty state. State does not grant permissions.

The example declares `current_page` and `selected_order_id` as strings and `form`
as JSON in `InvocationOptions.Contract`. Registration publishes the contract;
schema changes belong in the application and take effect after reconnecting.
Client state is enabled by default. An explicit `AllowClientState` pointer to false
rejects state and cannot be combined with a declared state contract.

In Playground, enable **Supply starting state** and edit the declared fields.
Unchecked fields are omitted. In datasets, each scenario owns its optional `state`
object. Values are concrete: no state profiles, merging, defaults, or token
substitution. Optional strings must be omitted instead of set to null.

Import `datasets/client-state.json` to exercise both supported application paths:
selected order DEMO-43 (processing), and form order DEMO-42 (shipped). Start an
evaluation using that dataset without a state profile. The exact response and
`lookup_order` trace are checked for each scenario.

The worker validates state against the pinned contract and retains it across turns
within that scenario session. Different scenario sessions do not share state.
Results preserve the submitted state, and the run preserves the contract version.
With no state, the example retains its DEMO-42 behavior. Explicit state without an
order fails. Logs show the resolved order ID rather than arbitrary state contents.

Existing profile snapshots remain available in historical runs; new runs use
scenario-owned state. The files in `profiles/` illustrate the retired profile API
and should not be supplied to new evaluations.
