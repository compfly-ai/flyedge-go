// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CompFly AI

package flyedge_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
)

func TestDecisionHandlerObservesTransportWarningWithoutRepeatingCheck(t *testing.T) {
	checks, observations := 0, 0
	warning := flyedge.Decision{Action: flyedge.ActionAllow, Warnings: []string{"review recipient"}, Message: "Review this message before sharing."}
	g := newGuard(t, flyedge.WithoutCloudTelemetry(), flyedge.WithEnforcer(seqEnforcer{seq: []enforce.Decision{warning}, i: &checks}),
		flyedge.WithDecisionHandler(func(req flyedge.CheckRequest, decision flyedge.Decision) {
			observations++
			if req.Stage != flyedge.StagePreLLM || req.SessionID != "warn-session" || req.RequestID == "" || req.Operation.ModelID != "gpt-4o-mini" {
				t.Errorf("missing check metadata: %+v", req)
			}
			if decision.Action != flyedge.ActionAllow || decision.Message != warning.Message || len(decision.Warnings) != 1 || decision.Warnings[0] != warning.Warnings[0] {
				t.Errorf("warning lost: %+v", decision)
			}
		}))
	defer g.Close()
	base := &fakeBase{}
	req, err := http.NewRequestWithContext(flyedge.ContextWithSession(context.Background(), "warn-session"), "POST", "https://api.openai.com/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := g.WrapRoundTripper(base).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if !base.called || observations != 1 || checks != 1 || g.Report().Checks != 1 {
		t.Fatalf("forwarded=%v observations=%d checks=%d telemetry=%+v", base.called, observations, checks, g.Report())
	}
}

func TestDecisionHandlerObservesFinalFailClosedDecision(t *testing.T) {
	calls := 0
	g := newGuard(t, flyedge.WithoutCloudTelemetry(), flyedge.WithFailMode(flyedge.FailClosed), flyedge.WithEnforcer(stubEnforcer{err: errors.New("unavailable")}), flyedge.WithDecisionHandler(func(_ flyedge.CheckRequest, decision flyedge.Decision) {
		calls++
		if decision.Action != flyedge.ActionDeny || decision.Reason != "enforcement_unavailable" {
			t.Errorf("not final decision: %+v", decision)
		}
	}))
	defer g.Close()
	_, err := g.Check(context.Background(), flyedge.CheckRequest{Stage: flyedge.StagePreLLM})
	if _, denied := flyedge.AsDenyError(err); !denied || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
