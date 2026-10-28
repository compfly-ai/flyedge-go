// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
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
