// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package otel_test

import (
	"context"
	"os"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	flyedge "github.com/compfly-ai/flyedge-go"
	feotel "github.com/compfly-ai/flyedge-go/telemetry/otel"
)

// TestLiveOTelExportsRealChecks runs REAL pre_llm checks against a running prism, with the OTel sink
// installed as the Guard's telemetry, and asserts each enforcement outcome became a flyedge.check
// span. Env-gated (FLYEDGE_LIVE=1) like the core live tests.
func TestLiveOTelExportsRealChecks(t *testing.T) {
	if os.Getenv("FLYEDGE_LIVE") == "" {
		t.Skip("set FLYEDGE_LIVE=1")
	}
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	cfg := flyedge.LoadEnv()
	cfg.Timeout = 20 * time.Second
	g, err := flyedge.New(cfg, flyedge.WithTelemetry(feotel.New(tp.Tracer("flyedge-live"))))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Close()

	ctx := context.Background()
	// Benign request — expect allow.
	_, _ = g.Check(ctx, flyedge.CheckRequest{
		SessionID: "otel-live", Stage: flyedge.StagePreLLM,
		ComponentType: "LLM", ComponentName: "api.anthropic.com", MethodName: "http",
		Content:   flyedge.Content{Full: "What is the capital of France?"},
		Operation: flyedge.Operation{Type: "chat.completions", ModelID: "claude-haiku-4-5", DestDomain: "api.anthropic.com"},
	})
	// Tool call to an external service — the local policy denies external_service access, so this
	// exercises the deny path and its span. (If policy allows it, the span still asserts fine.)
	_, _ = g.Check(ctx, flyedge.CheckRequest{
		SessionID: "otel-live", Stage: flyedge.StageToolCall,
		ComponentType: "TOOL", ComponentName: "fetch_url", MethodName: "call",
		Content:   flyedge.Content{Full: "fetch https://example.com"},
		Operation: flyedge.Operation{Type: "tool_call", ToolName: "fetch_url", DestDomain: "example.com"},
	})

	tp.ForceFlush(ctx)
	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2 (one per live check)", len(spans))
	}
	for _, s := range spans {
		attrs := map[string]string{}
		for _, kv := range s.Attributes() {
			attrs[string(kv.Key)] = kv.Value.Emit()
		}
		t.Logf("live span %q: action=%s stage=%s model=%s reason=%q status=%s",
			s.Name(), attrs["flyedge.action"], attrs["flyedge.stage"],
			attrs["gen_ai.request.model"], attrs["flyedge.reason"], s.Status().Code)
		if s.Name() != "flyedge.check" {
			t.Errorf("span name = %q", s.Name())
		}
		if attrs["flyedge.action"] == "" {
			t.Errorf("span missing flyedge.action")
		}
	}
}
