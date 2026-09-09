// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge_test

import (
	"context"
	"strings"
	"testing"

	flyedge "github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
)

// traceparentEnforcer captures the traceparent header the Guard hands the enforcer, which is the
// only place a caller can observe what prism will be told: the header's span field becomes the
// check's parent_span_id (prism mints the check's own span itself).
type traceparentEnforcer struct {
	traceparent string
	dec         enforce.Decision
}

func (e *traceparentEnforcer) Check(ctx context.Context, _ enforce.CheckRequest) (enforce.Decision, error) {
	e.traceparent = enforce.IdentityHeaders(ctx)["traceparent"]
	return e.dec, nil
}

func traceparentParts(t *testing.T, tp string) (traceID, spanID string) {
	t.Helper()
	parts := strings.Split(tp, "-")
	if len(parts) != 4 || parts[0] != "00" {
		t.Fatalf("malformed traceparent %q", tp)
	}
	if len(parts[1]) != 32 {
		t.Fatalf("traceparent trace id must be 32 hex, got %q in %q", parts[1], tp)
	}
	if len(parts[2]) != 16 {
		t.Fatalf("traceparent span id must be 16 hex, got %q in %q", parts[2], tp)
	}
	return parts[1], parts[2]
}

// A caller that names the span its operation is running under must see THAT span on the wire: it is
// what prism records as the check's parent, so substituting a locally minted one named a parent no
// other row carries and the check rendered as an orphan root instead of nesting under the operation
// it governs.
func TestCheckSendsCallerSpanAsTraceparentParent(t *testing.T) {
	rec := &traceparentEnforcer{dec: enforce.Decision{Action: flyedge.ActionAllow}}
	g, err := flyedge.New(flyedge.Config{}, flyedge.WithEnforcer(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const trace = "0123456789abcdef0123456789abcdef"
	const span = "fedcba9876543210"
	ctx := flyedge.ContextWithTrace(context.Background(), trace, span)
	if _, err := g.Check(ctx, flyedge.CheckRequest{SessionID: "sess-1", Stage: flyedge.StageToolCall}); err != nil {
		t.Fatalf("Check: %v", err)
	}

	gotTrace, gotSpan := traceparentParts(t, rec.traceparent)
	if gotTrace != trace {
		t.Errorf("trace id = %q, want the caller's %q", gotTrace, trace)
	}
	if gotSpan != span {
		t.Errorf("traceparent span = %q, want the caller's span %q", gotSpan, span)
	}
}

// With no enclosing span there is nothing to nest under, but prism records a check only when the
// traceparent carries some span (it pairs its minted span with that parent, both or neither), so a
// span is still minted. It must not leak the caller's trace, and must not be the caller's span.
func TestCheckMintsSpanWhenCallerHasNone(t *testing.T) {
	rec := &traceparentEnforcer{dec: enforce.Decision{Action: flyedge.ActionAllow}}
	g, err := flyedge.New(flyedge.Config{}, flyedge.WithEnforcer(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const trace = "0123456789abcdef0123456789abcdef"
	ctx := flyedge.ContextWithTrace(context.Background(), trace, "")
	if _, err := g.Check(ctx, flyedge.CheckRequest{SessionID: "sess-1", Stage: flyedge.StagePreLLM}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	gotTrace, first := traceparentParts(t, rec.traceparent)
	if gotTrace != trace {
		t.Errorf("trace id = %q, want the caller's %q", gotTrace, trace)
	}

	if _, err := g.Check(ctx, flyedge.CheckRequest{SessionID: "sess-1", Stage: flyedge.StagePreLLM}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	_, second := traceparentParts(t, rec.traceparent)
	if first == second {
		t.Errorf("minted span must be per-check, got %q twice", first)
	}
}

// No trace context at all keeps the pre-existing shape: a session-derived trace and a minted span.
func TestCheckWithoutTraceContextDerivesFromSession(t *testing.T) {
	rec := &traceparentEnforcer{dec: enforce.Decision{Action: flyedge.ActionAllow}}
	g, err := flyedge.New(flyedge.Config{}, flyedge.WithEnforcer(rec))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := g.Check(context.Background(), flyedge.CheckRequest{SessionID: "sess-1", Stage: flyedge.StagePreLLM}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	firstTrace, _ := traceparentParts(t, rec.traceparent)

	if _, err := g.Check(context.Background(), flyedge.CheckRequest{SessionID: "sess-1", Stage: flyedge.StagePreLLM}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	secondTrace, _ := traceparentParts(t, rec.traceparent)
	if firstTrace != secondTrace {
		t.Errorf("session-derived trace must be stable across checks: %q vs %q", firstTrace, secondTrace)
	}
}
