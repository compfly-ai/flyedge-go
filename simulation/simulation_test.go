// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package simulation_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	sim "github.com/compfly-ai/flyedge-go/simulation"
)

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestBehaviorFlags(t *testing.T) {
	cases := []struct {
		name string
		in   sim.BehaviorInput
		want string
	}{
		{"credential", sim.BehaviorInput{ComponentType: "tool", ComponentName: "http", ResultText: "api_key=supersecretvalue123"}, "credential_exposure"},
		{"external url in args", sim.BehaviorInput{ComponentType: "tool", ComponentName: "fetch", ArgsText: `{"url":"https://evil.example.com/x"}`}, "external_url_in_tool_args"},
		{"memory tool", sim.BehaviorInput{ComponentType: "tool", ComponentName: "save_memory", ArgsText: "{}"}, "memory_mutated"},
		{"code exec tool", sim.BehaviorInput{ComponentType: "tool", ComponentName: "bash", ArgsText: "{}"}, "code_executed"},
		{"privilege", sim.BehaviorInput{ComponentType: "tool", ComponentName: "db", ArgsText: "please DROP TABLE users"}, "privilege_escalation_pattern"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sim.Flags(c.in)
			if !contains(got, c.want) {
				t.Fatalf("Flags(%+v) = %v, want to contain %q", c.in, got, c.want)
			}
		})
	}
	// Benign input → no flags.
	if got := sim.Flags(sim.BehaviorInput{ComponentType: "tool", ComponentName: "get_weather", ArgsText: `{"city":"Paris"}`, ResultText: "18C"}); len(got) != 0 {
		t.Fatalf("benign input produced flags: %v", got)
	}
}

// The controller connects to the telemetry WS (with Bearer auth), streams the
// simulation_connected heartbeat, streams Record'd events with behavior flags, and
// deactivates cleanly.
func TestControllerStreamsToWebSocket(t *testing.T) {
	msgs := make(chan string, 64)
	authCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case authCh <- r.Header.Get("Authorization"):
		default:
		}
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

	c := sim.New("flyedge-go/test")
	c.OnConfigChange(&sim.Config{
		Active:       true,
		RunID:        "run-1",
		Middlewares:  []string{"telemetry", "behavior_monitor"},
		TelemetryJWT: "jwt-x",
		TelemetryURL: wsURL,
	})
	defer c.Stop()

	if !c.Active() || c.RunID() != "run-1" {
		t.Fatalf("expected active run-1; active=%v runID=%q", c.Active(), c.RunID())
	}

	// Heartbeat should arrive first.
	m := recv(t, msgs, 3*time.Second)
	if !strings.Contains(m, "simulation_connected") || !strings.Contains(m, `"run_id":"run-1"`) {
		t.Fatalf("first message not a heartbeat: %s", m)
	}
	if auth := recv(t, authCh, time.Second); auth != "Bearer jwt-x" {
		t.Fatalf("auth header = %q, want Bearer jwt-x", auth)
	}

	// A tool event with a flagged arg — behavior flags should be attached.
	c.Record(
		sim.RuntimeEvent{ComponentType: "tool", ComponentName: "send", ToolName: "send", ToolResult: "ok"},
		sim.BehaviorInput{ComponentType: "tool", ComponentName: "send", ArgsText: `{"url":"https://evil.example.com/x"}`},
	)
	deadline := time.Now().Add(3 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		m := recv(t, msgs, 3*time.Second)
		if strings.Contains(m, `"component_type":"tool"`) {
			if !strings.Contains(m, "external_url_in_tool_args") {
				t.Fatalf("tool event missing behavior flag: %s", m)
			}
			if !strings.Contains(m, `"run_id":"run-1"`) {
				t.Fatalf("tool event missing run_id: %s", m)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("did not receive the tool RuntimeEvent")
	}

	c.OnConfigChange(nil) // simulation removed
	if c.Active() {
		t.Fatal("controller still active after nil config")
	}
}

// Record is a no-op when no run is active (must not panic / must not send).
func TestRecordInactiveIsNoop(t *testing.T) {
	c := sim.New("t")
	c.Record(sim.RuntimeEvent{ComponentType: "tool", ComponentName: "x"}, sim.BehaviorInput{})
	if c.Active() {
		t.Fatal("controller should be inactive")
	}
}

func recv(t *testing.T, ch chan string, d time.Duration) string {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(d):
		t.Fatalf("timed out waiting for message")
		return ""
	}
}
