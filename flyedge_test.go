// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge_test

import (
	"context"
	"errors"
	"testing"

	"github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
)

// stubEnforcer returns a fixed decision or error — the explicit seam that makes Guard testable
// without a network or the live stack.
type stubEnforcer struct {
	dec enforce.Decision
	err error
}

func (s stubEnforcer) Check(context.Context, enforce.CheckRequest) (enforce.Decision, error) {
	return s.dec, s.err
}

func newGuard(t *testing.T, opts ...flyedge.Option) *flyedge.Guard {
	t.Helper()
	g, err := flyedge.New(flyedge.Config{}, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func TestCheckAllow(t *testing.T) {
	g := newGuard(t, flyedge.WithEnforcer(stubEnforcer{dec: enforce.Decision{Action: flyedge.ActionAllow}}))
	dec, err := g.Check(context.Background(), flyedge.CheckRequest{Stage: flyedge.StagePreLLM})
	if err != nil || dec.Action != flyedge.ActionAllow {
		t.Fatalf("allow: dec=%+v err=%v", dec, err)
	}
}

func TestCheckMergesEndpointAgentContext(t *testing.T) {
	recorder := &recordingEnforcer{dec: enforce.Decision{Action: flyedge.ActionAllow}}
	g := newGuard(t, flyedge.WithEnforcer(recorder))
	ctx := flyedge.ContextWithEndpointAgent(context.Background(), flyedge.EndpointAgent{
		InstanceKey: "claude-code\x00/Users/prakash/dev/payments-api",
	})
	if _, err := g.Check(ctx, flyedge.CheckRequest{Stage: flyedge.StagePreLLM}); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if recorder.last.EndpointAgent == nil || recorder.last.EndpointAgent.InstanceKey != "claude-code\x00/Users/prakash/dev/payments-api" {
		t.Fatalf("instance key missing from checked request: %+v", recorder.last.EndpointAgent)
	}
}

func TestCheckDenyReturnsDenyError(t *testing.T) {
	g := newGuard(t, flyedge.WithEnforcer(stubEnforcer{dec: enforce.Decision{Action: flyedge.ActionDeny, Reason: "jailbreak_detected"}}))
	dec, err := g.Check(context.Background(), flyedge.CheckRequest{Stage: flyedge.StagePreLLM})
	if dec.Action != flyedge.ActionDeny {
		t.Fatalf("want deny, got %+v", dec)
	}
	de, ok := flyedge.AsDenyError(err)
	if !ok {
		t.Fatalf("want *DenyError, got %v", err)
	}
	if de.Decision.Reason != "jailbreak_detected" {
		t.Errorf("deny reason = %q", de.Decision.Reason)
	}
}

func TestCheckWarnNoError(t *testing.T) {
	g := newGuard(t, flyedge.WithEnforcer(stubEnforcer{dec: enforce.Decision{Action: flyedge.ActionWarn}}))
	dec, err := g.Check(context.Background(), flyedge.CheckRequest{Stage: flyedge.StagePreLLM})
	if err != nil || dec.Action != flyedge.ActionWarn {
		t.Fatalf("warn: dec=%+v err=%v", dec, err)
	}
}

func TestReportAggregates(t *testing.T) {
	// A stub whose decision we flip per call, to exercise allow + deny counting in Report.
	seq := []enforce.Decision{
		{Action: flyedge.ActionAllow},
		{Action: flyedge.ActionAllow},
		{Action: flyedge.ActionDeny, Reason: "x"},
	}
	i := 0
	g := newGuard(t, flyedge.WithEnforcer(seqEnforcer{seq: seq, i: &i}))
	for range seq {
		_, _ = g.Check(context.Background(), flyedge.CheckRequest{Stage: flyedge.StagePreLLM})
	}
	s := g.Report()
	if s.Checks != 3 || s.Allowed != 2 || s.Denied != 1 {
		t.Fatalf("summary = %+v, want 3 checks / 2 allowed / 1 denied", s)
	}
	if s.ByStage["pre_llm"] != 3 {
		t.Errorf("byStage[pre_llm] = %d, want 3", s.ByStage["pre_llm"])
	}
}

type seqEnforcer struct {
	seq []enforce.Decision
	i   *int
}

func (s seqEnforcer) Check(context.Context, enforce.CheckRequest) (enforce.Decision, error) {
	d := s.seq[*s.i]
	*s.i++
	return d, nil
}

func TestFailOpenVsClosed(t *testing.T) {
	boom := errors.New("prism unreachable")

	// fail open (default): enforcement error → allow, nil error
	g := newGuard(t, flyedge.WithEnforcer(stubEnforcer{err: boom}))
	dec, err := g.Check(context.Background(), flyedge.CheckRequest{})
	if err != nil || dec.Action != flyedge.ActionAllow {
		t.Fatalf("fail-open: dec=%+v err=%v", dec, err)
	}

	// fail closed: enforcement error → deny + DenyError
	gc := newGuard(t, flyedge.WithEnforcer(stubEnforcer{err: boom}), flyedge.WithFailMode(flyedge.FailClosed))
	dec, err = gc.Check(context.Background(), flyedge.CheckRequest{})
	if dec.Action != flyedge.ActionDeny {
		t.Fatalf("fail-closed want deny, got %+v", dec)
	}
	if _, ok := flyedge.AsDenyError(err); !ok {
		t.Fatalf("fail-closed want *DenyError, got %v", err)
	}
}
