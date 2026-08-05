// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Package otel provides an OpenTelemetry telemetry sink for flyedge: it emits one OTel span per
// policy check to the caller's own tracer, so flyedge protection events (stage, allow/deny/warn,
// model, latency) surface in the caller's EXISTING observability pipeline — their collector,
// Datadog, Honeycomb, Grafana. This is orthogonal to the Compfly-native telemetry path
// (telemetry.Batched → /v1/flyedge/telemetry): that reports to Compfly; this reports to you.
//
// It is a separate module on purpose. The OpenTelemetry SDK is a heavyweight dependency, and the
// core flyedge library is deliberately zero-dependency — so OTel export is opt-in and lives here.
// The sink depends only on the OpenTelemetry *API* (go.opentelemetry.io/otel): the application owns
// the SDK TracerProvider, the exporter, and its shutdown. Wire it once and inject it:
//
//	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter)) // app owns this + tp.Shutdown
//	otel.SetTracerProvider(tp)
//	g, _ := flyedge.New(cfg, flyedge.WithTelemetry(feotel.New(nil))) // nil → global tracer
package otel

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	fetel "github.com/compfly-ai/flyedge-go/telemetry"
)

// instrumentationName identifies this instrumentation in the caller's telemetry.
const instrumentationName = "github.com/compfly-ai/flyedge-go"

// Telemetry is a flyedge telemetry.Telemetry that exports each policy check as an OpenTelemetry
// span. It also aggregates locally so Guard.Report() keeps working. Safe for concurrent Record.
type Telemetry struct {
	tracer trace.Tracer
	rec    *fetel.Recorder
}

// compile-time proof it satisfies the flyedge telemetry seam.
var _ fetel.Telemetry = (*Telemetry)(nil)

// New returns an OTel-exporting telemetry sink. Pass a specific tracer, or nil to use the global
// tracer from the caller's configured TracerProvider (otel.Tracer). The caller owns the provider
// and is responsible for its Shutdown/ForceFlush — Close here does not touch it.
func New(tracer trace.Tracer) *Telemetry {
	if tracer == nil {
		tracer = otel.Tracer(instrumentationName)
	}
	return &Telemetry{tracer: tracer, rec: fetel.NewRecorder()}
}

// Record emits a "flyedge.check" span for ev and folds it into the local Report aggregate. The span
// is timed from ev.OccurredAt for ev.LatencyMS when available, so it lands in the trace timeline.
// A deny/warn is carried as the flyedge.action attribute (not an error status) — only an actual
// enforcement-call failure (ev.Err) marks the span as an error, because a deny is the guard
// succeeding at its job, not a fault.
func (t *Telemetry) Record(ev fetel.Event) {
	t.rec.Record(ev)

	attrs := []attribute.KeyValue{
		attribute.String("flyedge.stage", ev.Stage),
		attribute.String("flyedge.action", ev.Action),
		attribute.Float64("flyedge.latency_ms", ev.LatencyMS),
	}
	if ev.Reason != "" {
		attrs = append(attrs, attribute.String("flyedge.reason", ev.Reason))
	}
	if ev.Model != "" {
		attrs = append(attrs, attribute.String("gen_ai.request.model", ev.Model))
	}

	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attrs...),
	}
	if !ev.OccurredAt.IsZero() {
		startOpts = append(startOpts, trace.WithTimestamp(ev.OccurredAt))
	}
	_, span := t.tracer.Start(context.Background(), "flyedge.check", startOpts...)

	if ev.Err != "" {
		span.RecordError(errors.New(ev.Err))
		span.SetStatus(codes.Error, ev.Err)
	} else {
		span.SetStatus(codes.Ok, "")
	}

	var endOpts []trace.SpanEndOption
	if !ev.OccurredAt.IsZero() && ev.LatencyMS > 0 {
		end := ev.OccurredAt.Add(time.Duration(ev.LatencyMS * float64(time.Millisecond)))
		endOpts = append(endOpts, trace.WithTimestamp(end))
	}
	span.End(endOpts...)
}

// Report returns the local aggregate — the same Summary the default in-memory sink would.
func (t *Telemetry) Report() fetel.Summary { return t.rec.Report() }

// Close is a no-op: the caller owns the TracerProvider and its shutdown/flush. Returning nil keeps
// Guard.Close well-behaved.
func (t *Telemetry) Close() error { return nil }
