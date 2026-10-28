// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge_test

import (
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
	"github.com/compfly-ai/flyedge-go/telemetry"
)

type checked struct{ session, traceparent string }

// toolEnforcer records each check's session and traceparent by tool name.
type toolEnforcer struct {
	mu     sync.Mutex
	byTool map[string]checked
}

func (e *toolEnforcer) Check(ctx context.Context, req enforce.CheckRequest) (enforce.Decision, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.byTool[req.Operation.ToolName] = checked{req.SessionID, enforce.IdentityHeaders(ctx)["traceparent"]}
	return enforce.Decision{Action: flyedge.ActionAllow}, nil
}

func TestInboundHandlerGivesChecksTheSimulationSession(t *testing.T) {
	rec := &toolEnforcer{byTool: map[string]checked{}}
	g := newGuard(t, flyedge.WithEnforcer(rec))
	agent := flyedge.InboundHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := g.CheckToolCall(r.Context(), "agent-session", r.URL.Query().Get("tool"), nil); err != nil {
			t.Errorf("CheckToolCall: %v", err)
		}
	}))
	trace := strings.Repeat("3", 32)
	requests := map[string]map[string]string{
		"eval":     {"X-Simulation-Session-Id": "eval:run:prompt", "Traceparent": "00-" + trace + "-" + strings.Repeat("4", 16) + "-01"},
		"pg":       {"X-Simulation-Session-Id": "pg-fixture"},
		"ordinary": {},
		"invalid":  {"X-Simulation-Session-Id": "ordinary"},
	}
	var wg sync.WaitGroup
	for tool, headers := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/?tool="+tool, nil)
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			agent.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()

	want := map[string]string{"eval": "eval:run:prompt", "pg": "pg-fixture", "ordinary": "agent-session", "invalid": "agent-session"}
	for tool, session := range want {
		if got := rec.byTool[tool].session; got != session {
			t.Errorf("%s: session = %q, want %q", tool, got, session)
		}
	}
	if !strings.Contains(rec.byTool["eval"].traceparent, trace) {
		t.Errorf("eval check did not nest under the inbound trace: %q", rec.byTool["eval"].traceparent)
	}
	if _, err := g.CheckToolCall(context.Background(), "agent-session", "unscoped", nil); err != nil || rec.byTool["unscoped"].session != "agent-session" {
		t.Errorf("a check outside the handler must keep its session: %+v %v", rec.byTool["unscoped"], err)
	}
}

type inboundTelemetry struct {
	telemetry.Recorder
	events []telemetry.Event
}

func (r *inboundTelemetry) Record(ev telemetry.Event) {
	r.events = append(r.events, ev)
}

func TestInboundHandlerTraceParent(t *testing.T) {
	const trace = "33333333333333333333333333333333"
	const parent = "4444444444444444"
	const valid = "00-" + trace + "-" + parent + "-01"
	for _, tc := range []struct {
		name             string
		sessions, traces []string
		wantSession      string
		wantParent       bool
	}{
		{"valid", []string{"eval:run:prompt"}, []string{valid}, "eval:run:prompt", true},
		{"missing session", nil, []string{valid}, "agent-session", false},
		{"invalid session", []string{"ordinary"}, []string{valid}, "agent-session", false},
		{"duplicate session", []string{"eval:run:prompt", "eval:other:prompt"}, []string{valid}, "agent-session", false},
		{"missing trace", []string{"eval:run:prompt"}, nil, "eval:run:prompt", false},
		{"malformed trace", []string{"eval:run:prompt"}, []string{"malformed"}, "eval:run:prompt", false},
		{"duplicate trace", []string{"eval:run:prompt"}, []string{valid, valid}, "eval:run:prompt", false},
		{"zero parent", []string{"eval:run:prompt"}, []string{"00-" + trace + "-0000000000000000-01"}, "eval:run:prompt", false},
		{"zero trace", []string{"eval:run:prompt"}, []string{"00-00000000000000000000000000000000-" + parent + "-01"}, "eval:run:prompt", false},
		{"invalid version", []string{"eval:run:prompt"}, []string{"ff-" + trace + "-" + parent + "-01"}, "eval:run:prompt", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &toolEnforcer{byTool: map[string]checked{}}
			tel := &inboundTelemetry{}
			g := newGuard(t, flyedge.WithEnforcer(rec), flyedge.WithTelemetry(tel))
			agent := flyedge.InboundHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := g.CheckToolCall(r.Context(), "agent-session", "tool", nil); err != nil {
					t.Errorf("CheckToolCall: %v", err)
				}
			}))
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			for _, session := range tc.sessions {
				req.Header.Add("X-Simulation-Session-Id", session)
			}
			for _, tp := range tc.traces {
				req.Header.Add("Traceparent", tp)
			}
			agent.ServeHTTP(httptest.NewRecorder(), req)
			got := rec.byTool["tool"]
			if got.session != tc.wantSession {
				t.Errorf("session = %q, want %q", got.session, tc.wantSession)
			}
			if len(tel.events) != 1 {
				t.Fatalf("telemetry events = %d, want 1", len(tel.events))
			}
			ev := tel.events[0]
			gotTrace, gotParent := traceparentParts(t, got.traceparent)
			if tc.wantParent {
				if gotTrace != trace || gotParent != parent {
					t.Errorf("outgoing traceparent = %q, want %q", got.traceparent, valid)
				}
				if ev.TraceID != trace || ev.ParentSpanID != parent {
					t.Errorf("telemetry trace/parent = %q/%q, want %q/%q", ev.TraceID, ev.ParentSpanID, trace, parent)
				}
			} else {
				if gotTrace == trace || gotParent == parent || ev.ParentSpanID != "" {
					t.Errorf("rejected inbound trace leaked: header=%q telemetry=%+v", got.traceparent, ev)
				}
			}
			if decoded, err := hex.DecodeString(ev.SpanID); err != nil || len(decoded) != 8 || ev.SpanID == "0000000000000000" || ev.SpanID == parent {
				t.Errorf("check span must be a distinct valid generated span: %q", ev.SpanID)
			}
		})
	}
}
