# flyedge OpenTelemetry sink

An opt-in `telemetry.Telemetry` that exports each flyedge policy check as an OpenTelemetry span, so
protection events show up in **your own** observability stack (collector, Datadog, Honeycomb,
Grafana). This is orthogonal to the Compfly-native path (`telemetry.Batched` →
`/v1/flyedge/telemetry`): that reports to Compfly, this reports to you. Run both if you want both.

It lives in its own module because the OTel SDK is a heavyweight dependency and the core `flyedge`
library is deliberately zero-dependency. The sink itself depends only on the OTel **API** — your
application owns the `TracerProvider`, the exporter, and its shutdown.

## Use

```go
import (
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	flyedge "github.com/compfly-ai/flyedge-go"
	feotel "github.com/compfly-ai/flyedge-go/telemetry/otel"
)

// You own the provider + exporter + shutdown.
tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
defer tp.Shutdown(ctx)
otel.SetTracerProvider(tp)

g, _ := flyedge.New(cfg, flyedge.WithTelemetry(feotel.New(nil))) // nil → global tracer
defer g.Close()
```

Each check emits a `flyedge.check` span with attributes:

| attribute | meaning |
|---|---|
| `flyedge.stage` | `pre_llm` / `tool_call` / `tool_call_response` / `post_llm` |
| `flyedge.action` | `allow` / `deny` / `warn` |
| `flyedge.reason` | policy reason (when present) |
| `flyedge.latency_ms` | enforcement latency |
| `gen_ai.request.model` | model id (when present) |

A **deny** is carried as `flyedge.action=deny` with an `Ok` span status — the guard did its job. Only
an actual enforcement-call failure (`Event.Err`) sets an `Error` status and records the error.
`Guard.Report()` keeps working: the sink aggregates locally as well.
