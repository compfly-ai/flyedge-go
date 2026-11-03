package flyedge

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/compfly-ai/flyedge-go/simulation"
)

func TestCanonicalInvocationCompletion(t *testing.T) {
	for _, outcome := range []string{"success", "error", "cancel", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			snapshots := make(chan map[string]any, 10)
			tr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer ws.CloseNow()
				for {
					_, b, err := ws.Read(ctx)
					if err != nil {
						return
					}
					var ev map[string]any
					_ = json.Unmarshal(b, &ev)
					if ev["event_type"] == "scenario.snapshot" {
						snapshots <- ev
					}
				}
			}))
			defer tr.Close()
			result := make(chan invocationFrame, 1)
			broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					_ = json.NewEncoder(w).Encode(map[string]any{"id": strings.Repeat("a", 32), "token": "token", "expires_at": time.Now().Add(time.Minute)})
					return
				}
				ws, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer ws.CloseNow()
				_ = wsjson.Write(ctx, ws, invocationFrame{Type: "ready"})
				_ = wsjson.Write(ctx, ws, invocationFrame{Type: "invoke", Request: &InvocationRequest{ID: "one", Purpose: PurposeEval, RunID: "run", SessionID: "session", Messages: []InvocationMessage{{Role: "user", Content: "test"}}, Deadline: time.Now().Add(time.Second)}})
				var frame invocationFrame
				_ = wsjson.Read(ctx, ws, &frame)
				result <- frame
				_, _, _ = ws.Read(ctx)
			}))
			defer broker.Close()
			_, priv, _ := ed25519.GenerateKey(nil)
			der, _ := x509.MarshalPKCS8PrivateKey(priv)
			g, err := New(Config{APIURL: broker.URL, DID: "did:compfly:org:agent", KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})})
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			g.simCtl = simulation.New("test")
			g.simCtl.OnConfigChange(&simulation.Config{Active: true, RunID: "run", TelemetryJWT: "evidence", TelemetryURL: "ws" + strings.TrimPrefix(tr.URL, "http"), Extra: json.RawMessage(`{"scenario_overrides":{"id":"p","session_id":"session","step_id":"ask","overrides":[]}}`)})
			_, err = g.ServeInvocations(ctx, InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposeEval}, AllowInsecureLoopback: true, LeaseDuration: time.Minute}, func(ctx context.Context, _ InvocationRequest, _ InvocationWriter) error {
				switch outcome {
				case "error":
					return errors.New("failed")
				case "panic":
					panic("test")
				case "cancel":
					<-ctx.Done()
					return nil
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case frame := <-result:
				want := "failed"
				if outcome == "success" {
					want = "completed"
				}
				if frame.Type != want {
					t.Fatalf("frame: %+v", frame)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			for {
				select {
				case ev := <-snapshots:
					if ev["scenario_phase"] != "complete" {
						continue
					}
					if ev["scenario_success"] != (outcome == "success") {
						t.Fatal(ev)
					}
					return
				case <-ctx.Done():
					t.Fatal("missing completion snapshot")
				}
			}
		})
	}
}

func TestScenarioGuardAuthorizationAndRealEvidence(t *testing.T) {
	g, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.simCtl = simulation.New("test")
	g.simCtl.OnConfigChange(&simulation.Config{Active: true, RunID: "run", TelemetryJWT: "token", TelemetryURL: "ws://127.0.0.1:1", Extra: json.RawMessage(`{"scenario_overrides":{"id":"p","session_id":"s","step_id":"ask","overrides":[{"id":"r","tool":"read","occurrence":1,"arguments":{},"value":"mock","required_matches":1}]}}`)})
	ctx := ContextWithSession(context.Background(), "s")
	for _, scope := range []invocationScope{{runID: "wrong", allowInjection: true}, {runID: "run", allowInjection: false}} {
		if _, _, err := g.BeforeScenarioTool(context.WithValue(ctx, invocationContextKey{}, scope), "s", "read", map[string]any{}, true); err == nil {
			t.Fatal("unauthorized mock")
		}
	}
	ctx = context.WithValue(ctx, invocationContextKey{}, invocationScope{runID: "run", allowInjection: true})
	if _, _, err := g.BeforeScenarioTool(ctx, "other", "read", map[string]any{}, true); err == nil {
		t.Fatal("wrong session accepted")
	}
	if _, _, err := g.BeforeScenarioTool(ctx, "s", "read", map[string]any{}, false); err == nil {
		t.Fatal("deployment opt out ignored")
	}
	g.recordSimEvent(CheckRequest{SessionID: "s", Stage: StageToolCall, Operation: Operation{ToolName: "read", ToolArgsJSON: `{}`}})
	raw, mocked, err := g.BeforeScenarioTool(ctx, "s", "read", map[string]any{}, true)
	if err != nil || !mocked || string(raw) != `"mock"` {
		t.Fatal(string(raw), mocked, err)
	}
	g.RecordToolIO("s", "req", "real-tool", `{}`, `{"ok":true}`)
	ev := g.ScenarioMockStatus()["events"].([]any)
	want := []string{"component.requested", "component.mocked", "component.executed"}
	if len(ev) != len(want) {
		t.Fatal(ev)
	}
	for i, k := range want {
		if ev[i].(map[string]any)["event"] != k {
			t.Fatal(ev)
		}
	}
}
