# Remote support agent

An explicit, outbound-only FlyEdge connection lets Playground and simulations invoke
this handler without a public listener or tunnel. This deterministic example uses
one read-only `lookup_order` fixture and real SDK policy checks and telemetry.
It does not use a model provider; evaluation scores measure the fixture, not model resilience.

The example logs each invocation to stderr: conversation messages, policy stages,
requested and executed tool calls, raw and governed tool results, and delivered
output. Every event includes invocation, purpose, run and session IDs; completion
includes duration, and failures include the stage and error. Content is logged in
full, including simulation-injected content, so use this with test data. Log values
are quoted/escaped so multiline input stays within its event.

Restart `go run ./remote-agent ...` after changing the example to load the new code.

Register a dedicated agent and generate its DID identity through Platform. Set:

```sh
export COMPFLY_API_URL=https://your-prism
export COMPFLY_AGENT_DID=did:compfly:...
export COMPFLY_AGENT_PRIVATE_KEY_PATH=/private/path/agent.pem
cd examples
go run ./remote-agent -environment dev
```

For the local Tilt stack, use `http://127.0.0.1:8080`, set
`COMPFLY_SIM_TELEMETRY_URL=ws://127.0.0.1:8080/v1/simulation/telemetry` if needed,
and explicitly pass `-allow-loopback-http -allow-simulation-injection`.
The injection flag permits the existing simulation hooks on this dedicated process.
Unprotected baseline runs are rejected. Use shielded simulation runs.

Platform discovers the lease by authenticated organization, agent slug, environment
and purpose. The existing persisted HTTP endpoint is not changed. The agent must
already exist in the evaluation catalog; this API does not create identities.

V1 supports one serving connection per agent/environment and one invocation at a time.
SDK output streams over the socket; the Platform adapter buffers the final response.
Disconnects are terminal with potentially unknown in-flight outcomes: no automatic
reconnect, retry or replay. Restart explicitly to authorize a new session. Platform
can revoke the lease; closing the Guard also closes the connection. Cancellation is
cooperative and cannot undo side effects. No acting-principal/state-profile support yet.

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
go run ./remote-agent -environment dev -allow-loopback-http -allow-simulation-injection
```

Run from the `examples` directory with the local stack up. Ctrl+C stops serving.
The temporary identity files are not portable or committed; generate a new identity
through Platform when recreating the fixture on another machine.
