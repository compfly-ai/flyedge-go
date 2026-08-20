# OTel example — export flyedge protection events as OpenTelemetry spans

A flyedge-protected Claude agent whose policy checks are exported as `flyedge.check` OpenTelemetry
spans. It wires a **stdout** exporter so you can watch the spans print; in production you'd swap in
an OTLP exporter to your collector (Datadog, Honeycomb, Grafana) — the flyedge sink doesn't change.

This is the customer-side observability path (`telemetry/otel`), independent of the Compfly-native
telemetry path. Your app owns the `TracerProvider` + exporter + shutdown; the core library stays
zero-dependency.

## Run

```bash
export COMPFLY_API_URL=https://prism.p.compfly.ai   # your CompFly gateway (defaults to this when unset)
export COMPFLY_AGENT_DID=did:compfly:...             # from registering an agent in the CompFly platform
export COMPFLY_AGENT_PRIVATE_KEY_PATH=/path/to/agent.pem
export ANTHROPIC_API_KEY=sk-ant-...

go run ./otel/
```

You'll see the reply, the local `Report()` summary, then the exported span(s):

```
flyedge guard: DID=did:compfly:... (OTel telemetry → stdout)
prompt="What are your store hours?"
reply: ...
flyedge: 1 checks — 1 allowed, 0 denied, 0 warned, 0 errors (21.1ms total)
--- flyedge.check spans (exported on shutdown) ---
{ "Name": "flyedge.check", "Attributes": [ {flyedge.stage: pre_llm}, {flyedge.action: allow},
  {gen_ai.request.model: claude-haiku-4-5}, {flyedge.latency_ms: ...} ], ... }
```

A policy **deny** shows up as a span with `flyedge.action=deny` (and the model is never called).
