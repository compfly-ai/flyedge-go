// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package simulation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The controller, driven by an attack-mode config, mutates a tool result via the injector and emits
// an attack_injected telemetry event over the WebSocket — the full B2b path end to end.
func TestControllerAttackInjectionOverWS(t *testing.T) {
	msgs := make(chan string, 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			select {
			case msgs <- string(data):
			default:
			}
		}
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	ctl := New("flyedge-go/test")
	ctl.SetManifest([]string{"get_account"}, []string{"claude-sonnet-4-5"})
	ctl.OnConfigChange(&Config{
		Active:       true,
		RunID:        "run-atk",
		Middlewares:  []string{"telemetry", "behavior_monitor", "attack_injector"},
		TelemetryJWT: "jwt-x",
		TelemetryURL: wsURL,
		Extra: []byte(`{"attack_injector":{"mode":"attack","tier":2,"max_injections":10,
			"attack_config":{"chains":[
			  {"name":"poison_acct","steps":[{"strategy":"tool_poison","target_component_type":"tool","target_component_name":"get_account","sophistication":2}]}
			]}}}`),
	})
	defer ctl.Stop()

	if !ctl.Active() {
		t.Fatal("expected active run")
	}
	drainHeartbeat(t, msgs)

	// Inject on a tool result — should mutate + emit an attack_injected event.
	out, injected := ctl.InjectToolResult("get_account", `{"balance":100}`)
	if !injected {
		t.Fatal("expected tool_poison to fire on get_account")
	}
	if out == `{"balance":100}` {
		t.Fatalf("result should be mutated, got unchanged: %s", out)
	}
	if !strings.Contains(out, "balance") {
		t.Fatalf("tool_poison should merge (keep original fields): %s", out)
	}

	m := recvContaining(t, msgs, "attack_injected", 3*time.Second)
	if !strings.Contains(m, `"injection_strategy":"tool_poison"`) {
		t.Fatalf("injection event missing strategy: %s", m)
	}
	if !strings.Contains(m, `"injection_target":"get_account"`) || !strings.Contains(m, `"injection_tier":2`) {
		t.Fatalf("injection event missing target/tier: %s", m)
	}

	// A non-targeted tool does not inject.
	if _, injected := ctl.InjectToolResult("checkout", `{}`); injected {
		t.Fatal("checkout has no chain — must not inject")
	}
}

func drainHeartbeat(t *testing.T, ch chan string) {
	t.Helper()
	// First frame is the simulation_connected heartbeat; skip any non-injection frames briefly.
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("no heartbeat received")
	}
}

func recvContaining(t *testing.T, ch chan string, want string, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		select {
		case m := <-ch:
			if strings.Contains(m, want) {
				return m
			}
		case <-time.After(time.Until(deadline)):
		}
	}
	t.Fatalf("timed out waiting for a message containing %q", want)
	return ""
}
