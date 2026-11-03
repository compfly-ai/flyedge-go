package flyedge

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/compfly-ai/flyedge-go/simulation"
)

// Exercise admission, policy checks, the pre-tool hook, and result governance
// together so an observation exception cannot stop at the admission gate.
func TestInvocationLiveToolScenarios(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		active, baseline                 bool
		run, overrides, extra, wantError string
		deny                             bool
	}{
		{name: "inactive without plan", run: "run"},
		{name: "inactive observation", run: "run", overrides: `[]`},
		{name: "active observation", active: true, run: "run", overrides: `[]`},
		{name: "active without plan", active: true, run: "run"},
		{name: "active disabled override", active: true, run: "run", overrides: `[{"id":"r","tool":"read","occurrence":1,"arguments":{},"value":"mock","required_matches":1,"enabled":false}]`},
		{name: "enabled override denied", active: true, run: "run", overrides: `[{"id":"r","tool":"read","occurrence":1,"arguments":{},"value":"mock","required_matches":1}]`, wantError: "scenario plan requires authorized simulation scope"},
		{name: "baseline denied", active: true, baseline: true, run: "run", overrides: `[]`, wantError: "unprotected baseline not authorized"},
		{name: "wrong run denied", active: true, run: "other", overrides: `[]`, wantError: "active simulation belongs to another run"},
		{name: "attack mode denied", active: true, run: "run", overrides: `[]`, extra: `{"attack_injector":{"mode":"attack"}}`, wantError: "simulation hooks require explicit permission"},
		{name: "global mock denied", active: true, run: "run", overrides: `[]`, extra: `{"scenario_overrides":{"id":"global","session_id":"eval:run:s","step_id":"ask","overrides":[{"id":"r","tool":"read","occurrence":1,"arguments":{},"value":"mock","required_matches":1}]}}`, wantError: "simulation hooks require explicit permission"},
		{name: "policy denial retained", active: true, run: "run", overrides: `[]`, deny: true, wantError: "policy denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			session := "eval:" + tc.run + ":s"
			var plan json.RawMessage
			if tc.overrides != "" {
				plan = json.RawMessage(fmt.Sprintf(`{"id":"p","session_id":%q,"step_id":"ask","overrides":%s}`, session, tc.overrides))
			}
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
				_ = wsjson.Write(ctx, ws, invocationFrame{Type: "invoke", Request: &InvocationRequest{ID: "one", Purpose: PurposeEval, RunID: tc.run, SessionID: session, Messages: []InvocationMessage{{Role: "user", Content: "test"}}, Deadline: time.Now().Add(time.Second), ScenarioPlan: plan}})
				var f invocationFrame
				if err := wsjson.Read(ctx, ws, &f); err != nil {
					t.Error(err)
					return
				}
				result <- f
				_, _, _ = ws.Read(ctx)
			}))
			defer broker.Close()
			_, priv, _ := ed25519.GenerateKey(nil)
			der, _ := x509.MarshalPKCS8PrivateKey(priv)
			checker := &mediaChecker{decision: Decision{Action: ActionAllow}}
			if tc.deny {
				checker.decision = Decision{Action: ActionDeny, Reason: "policy denied"}
			}
			g, err := New(Config{APIURL: broker.URL, DID: "did:compfly:org:agent", KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}, WithEnforcer(checker))
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			g.simCtl = simulation.New("test")
			if tc.active {
				g.simCtl.OnConfigChange(&simulation.Config{Active: true, RunID: "run", ProtectionDisabled: tc.baseline, TelemetryJWT: "token", TelemetryURL: "ws://127.0.0.1:1", Extra: json.RawMessage(tc.extra)})
				if !g.simCtl.Active() {
					t.Fatal("controller not active")
				}
			}
			allow := false
			_, err = g.ServeInvocations(ctx, InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposeEval}, AllowInsecureLoopback: true, AllowSimulationInjection: &allow}, func(ctx context.Context, r InvocationRequest, _ InvocationWriter) error {
				if _, err := g.CheckToolCall(ctx, r.SessionID, "read", map[string]any{}); err != nil {
					return err
				}
				_, mocked, err := g.BeforeScenarioTool(ctx, r.SessionID, "read", map[string]any{}, false)
				if err != nil {
					return err
				}
				if mocked {
					return fmt.Errorf("unexpected mock")
				}
				g.RecordToolIO(r.SessionID, "req", "read", `{}`, `"live"`)
				result, _, err := g.GovernToolResult(ctx, r.SessionID, "read", `"live"`)
				if err == nil && result != `"live"` {
					return fmt.Errorf("live result mutated: %s", result)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case f := <-result:
				if tc.wantError != "" {
					if f.Type != "failed" || !strings.Contains(f.Error, tc.wantError) {
						t.Fatalf("expected %q; got %+v", tc.wantError, f)
					}
				} else {
					if f.Type != "completed" {
						t.Fatalf("expected live completion; got %+v", f)
					}
					if len(checker.requests) != 2 {
						t.Fatalf("policy checks = %d; want tool and result", len(checker.requests))
					}
					if plan != nil && (f.ScenarioSnapshot["complete"] != true || f.ScenarioSnapshot["handler_success"] != true) {
						t.Fatalf("incomplete evidence: %+v", f.ScenarioSnapshot)
					}
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}
