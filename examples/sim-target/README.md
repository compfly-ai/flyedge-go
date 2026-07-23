# sim-target — an engine-drivable flyedge-go agent

`sim-target` is a minimal flyedge-go agent that an **evaluation engine can actively drive**.
It serves a Guard-wrapped, OpenAI-compatible chat endpoint; every inbound turn runs the Guard's
`Check` stages, so while a simulation run is active the engine's turns become the `RuntimeEvent`
telemetry stream the platform correlates.

```
eval engine ──POST /v1/chat/completions (turn)──▶ sim-target (Guard-wrapped handler)
                                                     │  Check(pre_llm) … Check(post_llm)
                                                     ▼
                              RuntimeEvents ──WS──▶ prism ──▶ sim:telemetry:{runId} ──▶ eval-runner
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

1. A running gateway (prism). Locally: `just local` in `terraform-compfly/local` (k3d + Tilt).
2. A **registered agent with a DID identity** — see below.
3. Go 1.26+.

## First-time setup: register the agent + mint its DID

The agent authenticates to the gateway with an Ed25519 DID identity. Get one of two ways:

### Via the CompFly MCP (works against any environment)

Ask Claude (or any MCP client with the CompFly server configured):

1. `register_agent` — `{ "slug": "my-sim-agent", "name": "My Sim Agent" }`
2. `generate_agent_identity` — `{ "id": "my-sim-agent" }` → returns `did` + `privateKeyPem`.
   **The private key is shown once** — save it to a file (e.g. `keys/my-sim-agent.pem`, `chmod 600`).
3. *(optional)* `enable_agent_enforcement` — `{ "agentSlug": "my-sim-agent" }` to put it under policy.

Then point the runner at them (see Configuration).

### Via the local k3d stack (dev shortcut)

```bash
AGENT_SLUG=my-sim-agent bash terraform-compfly/local/scripts/register-host-agent.sh
```

Registers the agent (idempotent), mints/rotates the identity, and writes
`terraform-compfly/local/keys/my-sim-agent.{did,pem}` — no browser needed.

## Configuration

Copy `.env.example` → `.env` (gitignored) and fill in, or export the vars directly:

| Var | Meaning | Default |
|---|---|---|
| `COMPFLY_API_URL` | prism gateway base | `http://localhost:8080` |
| `COMPFLY_AGENT_DID` | the agent's DID | *(required)* |
| `COMPFLY_AGENT_PRIVATE_KEY_PATH` | Ed25519 PEM path | *(required)* |
| `SIM_TARGET_ADDR` | HTTP listen address | `:8899` |
| `COMPFLY_SIM_TELEMETRY_URL` | split-horizon telemetry override (local dev only) | *(unset)* |

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

## Split-horizon note (local dev)

If you run the agent on your **host** but the gateway runs **in a cluster**, the gateway may advertise
an in-cluster telemetry URL (e.g. `ws://prism:8080/...`) your host can't resolve, and the engine
reaches your endpoint at the host's gateway IP (on k3d/Docker-Desktop, `host.docker.internal` ≈
`192.168.5.2`). Set `COMPFLY_SIM_TELEMETRY_URL=ws://localhost:8080/v1/simulation/telemetry` so the
telemetry WebSocket uses your port-forwarded gateway. This override is off by default (the agent is
server-authoritative); it exists only for this local split-horizon case.
