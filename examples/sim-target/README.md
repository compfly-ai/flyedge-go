# sim-target — an engine-drivable flyedge-go agent

`sim-target` is a minimal flyedge-go agent that an **evaluation engine can actively drive**.
It serves a Guard-wrapped, OpenAI-compatible chat endpoint; every inbound turn runs the Guard's
`Check` stages, so while a simulation run is active the engine's turns become the `RuntimeEvent`
telemetry stream the platform correlates.

```
eval engine ──POST /v1/chat/completions (turn)──▶ sim-target (Guard-wrapped handler)
                                                     │  Check(pre_llm) … Check(post_llm)
                                                     ▼
                              RuntimeEvents ──WS──▶ CompFly platform (Simulation Lab)
```

The endpoint is how the engine drives the agent; the telemetry WebSocket is how it sees inside it.
Two halves of one instrumented target.

## Endpoint contract

- `POST /v1/chat/completions` — body `{"messages":[{"role","content"}...],"session_id":"..."}`,
  returns `{"choices":[{"message":{"role":"assistant","content":"..."}}]}` (OpenAI-compatible).
- `GET /health` → `200 {"status":"ok"}`.

The reply is a deterministic offline stand-in (a real target would call its LLM through the Guard's
transport wrap here). A policy denial at `pre_llm` turns into a refusal — the guarded-agent behavior
an eval scores.

## Prerequisites

1. A reachable gateway (prism) at `COMPFLY_API_URL` (defaults to `https://prism.p.compfly.ai`).
2. A **registered agent with a DID identity** — see below.
3. Go 1.23+.

## First-time setup: register the agent + mint its DID

The agent authenticates to the gateway with an Ed25519 DID identity. Register an agent and mint its
identity in the CompFly platform:

### Via the CompFly MCP (works against any environment)

Ask Claude (or any MCP client with the CompFly server configured):

1. `register_agent` — `{ "slug": "my-sim-agent", "name": "My Sim Agent" }`
2. `generate_agent_identity` — `{ "id": "my-sim-agent" }` → returns `did` + `privateKeyPem`.
   **The private key is shown once** — save it to a file (e.g. `keys/my-sim-agent.pem`, `chmod 600`).
3. *(optional)* `enable_agent_enforcement` — `{ "agentSlug": "my-sim-agent" }` to put it under policy.

Then point the runner at them (see Configuration).

## Configuration

Copy `.env.example` → `.env` (gitignored) and fill in, or export the vars directly:

| Var | Meaning | Default |
|---|---|---|
| `COMPFLY_API_URL` | prism gateway base | `https://prism.p.compfly.ai` |
| `COMPFLY_AGENT_DID` | the agent's DID | *(required)* |
| `COMPFLY_AGENT_PRIVATE_KEY_PATH` | Ed25519 PEM path | *(required)* |
| `SIM_TARGET_ADDR` | HTTP listen address | `:8899` |
| `COMPFLY_SIM_TELEMETRY_URL` | advanced telemetry-WS override (normally unset) | *(unset)* |

## Run

```bash
./run.sh
```

Leave it running. It connects to the gateway, serves the chat endpoint, and — when a simulation
run is started against it — streams each turn's Checks as telemetry.

## Drive an evaluation against it

Once it's up and the agent is registered as an eval target with this endpoint, start a run — from
the platform UI (Simulation Lab → Run Eval), or the eval API. A red_team run generates attack
scenarios, POSTs them to the endpoint, and scores the responses while the Guard streams the internal
telemetry.

## Telemetry WebSocket override (advanced)

`COMPFLY_SIM_TELEMETRY_URL` lets you pin the telemetry WebSocket to a specific URL instead of the one
the gateway advertises. It is off by default (the agent is server-authoritative) and should stay
unset in normal deployments; it exists only for unusual network setups where the advertised URL isn't
reachable from where the agent runs.
