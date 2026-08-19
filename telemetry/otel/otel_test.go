// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package otel_test

import (
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	fetel "github.com/compfly-ai/flyedge-go/telemetry"
	feotel "github.com/compfly-ai/flyedge-go/telemetry/otel"
)

// newRecordingSink wires an in-memory SpanRecorder so the test can inspect emitted spans.
func newRecordingSink() (*feotel.Telemetry, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return feotel.New(tp.Tracer("test")), sr
}

func TestRecordEmitsSpanWithAttributes(t *testing.T) {
	sink, sr := newRecordingSink()
	sink.Record(fetel.Event{
		Stage:      "pre_llm",
		Action:     "allow",
		Model:      "claude-haiku-4-5",
		LatencyMS:  12.5,
		OccurredAt: time.Unix(1700000000, 0),
	})

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "flyedge.check" {
		t.Errorf("span name = %q, want flyedge.check", s.Name())
	}
	attrs := map[string]string{}
	for _, kv := range s.Attributes() {
		attrs[string(kv.Key)] = kv.Value.Emit()
	}
	if attrs["flyedge.stage"] != "pre_llm" {
		t.Errorf("flyedge.stage = %q", attrs["flyedge.stage"])
	}
	if attrs["flyedge.action"] != "allow" {
		t.Errorf("flyedge.action = %q", attrs["flyedge.action"])
	}
	if attrs["gen_ai.request.model"] != "claude-haiku-4-5" {
		t.Errorf("gen_ai.request.model = %q", attrs["gen_ai.request.model"])
	}
	if _, ok := attrs["flyedge.latency_ms"]; !ok {
		t.Errorf("missing flyedge.latency_ms attribute")
	}
}

func TestDenyIsNotAnErrorSpanButErrIs(t *testing.T) {
	sink, sr := newRecordingSink()
	sink.Record(fetel.Event{Stage: "pre_llm", Action: "deny", Reason: "policy", OccurredAt: time.Unix(1700000000, 0)})
	sink.Record(fetel.Event{Stage: "pre_llm", Action: "allow", Err: "enforcement_unavailable", OccurredAt: time.Unix(1700000000, 0)})

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	// deny → Ok status (guard did its job); enforcement error → Error status.
	if spans[0].Status().Code.String() != "Ok" {
		t.Errorf("deny span status = %v, want Ok", spans[0].Status().Code)
	}
	if spans[1].Status().Code.String() != "Error" {
		t.Errorf("error span status = %v, want Error", spans[1].Status().Code)
	}
	if len(spans[1].Events()) == 0 {
		t.Errorf("expected a recorded error event on the enforcement-error span")
	}
}

func TestReportStillAggregates(t *testing.T) {
	sink, _ := newRecordingSink()
	sink.Record(fetel.Event{Stage: "pre_llm", Action: "allow"})
	sink.Record(fetel.Event{Stage: "pre_llm", Action: "deny"})
	sum := sink.Report()
	if sum.Checks != 2 || sum.Allowed != 1 || sum.Denied != 1 {
		t.Errorf("summary = %+v, want 2 checks / 1 allowed / 1 denied", sum)
	}
	if err := sink.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}
