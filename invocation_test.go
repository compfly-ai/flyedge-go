package flyedge

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/compfly-ai/flyedge-go/simulation"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestInvocationRequiresExplicitIdentityAndScope(t *testing.T) {
	g, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	_, err = g.ServeInvocations(context.Background(), InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}, LeaseDuration: time.Minute}, func(context.Context, InvocationRequest, InvocationWriter) error { return nil })
	if err == nil {
		t.Fatal("unsigned guard must not expose an invocation handler")
	}
}

func TestInvocationOptionsRejectImplicitScope(t *testing.T) {
	for _, opts := range []InvocationOptions{
		{},
		{Environment: "dev", LeaseDuration: time.Minute},
		{Environment: "dev", Purposes: []InvocationPurpose{"unknown"}, LeaseDuration: time.Minute},
		{Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}, LeaseDuration: 25 * time.Hour},
	} {
		if _, err := opts.validate(); err == nil {
			t.Fatalf("accepted invalid scope: %+v", opts)
		}
	}
}

func invocationFixture(t *testing.T, peer func(context.Context, *websocket.Conn)) (*Guard, *InvocationConnection) {
	return invocationLeaseFixture(t, time.Minute, nil, peer)
}
func invocationLeaseFixture(t *testing.T, duration time.Duration, autoRenew *bool, peer func(context.Context, *websocket.Conn)) (*Guard, *InvocationConnection) {
	return invocationStateFixture(t, duration, autoRenew, nil, peer)
}
func invocationStateFixture(t *testing.T, duration time.Duration, autoRenew *bool, allowState *bool, peer func(context.Context, *websocket.Conn)) (*Guard, *InvocationConnection) {
	return invocationHistoryFixture(t, duration, autoRenew, allowState, false, peer)
}
func invocationHistoryFixture(t *testing.T, duration time.Duration, autoRenew *bool, allowState *bool, allowHistory bool, peer func(context.Context, *websocket.Conn)) (*Guard, *InvocationConnection) {
	return invocationCapacityFixture(t, duration, autoRenew, allowState, allowHistory, 1, peer)
}
func invocationCapacityFixture(t *testing.T, duration time.Duration, autoRenew *bool, allowState *bool, allowHistory bool, capacity int, peer func(context.Context, *websocket.Conn), mockPlans ...bool) (*Guard, *InvocationConnection) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	_, priv, _ := ed25519.GenerateKey(nil)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var registration struct {
				AllowClientState *bool `json:"allow_client_state"`
			}
			if err := json.NewDecoder(r.Body).Decode(&registration); err != nil {
				t.Error(err)
			}
			wantState := allowState == nil || *allowState
			if registration.AllowClientState == nil || *registration.AllowClientState != wantState {
				t.Errorf("registration client state = %v, want %v", registration.AllowClientState, wantState)
			}
			if r.Header.Get("X-CompFly-Signature") == "" {
				t.Error("missing DID signature")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": strings.Repeat("a", 32), "token": "test-token", "expires_at": time.Now().Add(duration)})
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing lease token")
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.CloseNow()
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "ready"})
		peer(ctx, ws)
	}))
	t.Cleanup(server.Close)
	g, err := New(Config{APIURL: server.URL, DID: "did:compfly:org:agent", KeyPEM: key})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	if len(mockPlans) > 0 && mockPlans[0] {
		g.simCtl = simulation.New("test")
	}
	allowInjection := len(mockPlans) > 0 && mockPlans[0]
	c, err := g.ServeInvocations(ctx, InvocationOptions{AllowSimulationInjection: &allowInjection, MaxConcurrentInvocations: capacity, AllowClientHistory: allowHistory, AllowClientState: allowState, Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}, LeaseDuration: duration, AutoRenew: autoRenew, AllowInsecureLoopback: true}, func(ctx context.Context, r InvocationRequest, w InvocationWriter) error {
		if r.SessionID == "" || sessionFromContext(ctx) != r.SessionID {
			return errors.New("missing session context")
		}
		if len(r.ScenarioPlan) > 0 {
			value, mocked, err := g.BeforeScenarioTool(ctx, r.SessionID, "read", map[string]any{}, true)
			if err != nil {
				return err
			}
			if !mocked {
				return errors.New("mock missing")
			}
			if err := w.TextDelta(ctx, string(value)); err != nil {
				return err
			}
			if r.Messages[0].Content == "wait" {
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		}
		if r.Messages[0].Content == "wait" {
			<-ctx.Done()
			return ctx.Err()
		}
		if r.History != nil {
			data, _ := json.Marshal(r.History)
			return w.TextDelta(ctx, string(data))
		}
		if r.State != nil {
			data, err := json.Marshal(r.State)
			if err != nil {
				return err
			}
			return w.TextDelta(ctx, string(data))
		}
		return w.TextDelta(ctx, "hello")
	})
	if err != nil {
		t.Fatal(err)
	}
	return g, c
}
func sendInvocation(ctx context.Context, ws *websocket.Conn, id, prompt string, purpose InvocationPurpose) {
	_ = wsjson.Write(ctx, ws, invocationFrame{Type: "invoke", Request: &InvocationRequest{ID: id, Purpose: purpose, SessionID: "session", Messages: []InvocationMessage{{Role: "user", Content: prompt}}, Deadline: time.Now().Add(time.Minute)}})
}
func TestInvocationStreamsThenRevocationIsTerminal(t *testing.T) {
	_, c := invocationFixture(t, func(ctx context.Context, ws *websocket.Conn) {
		sendInvocation(ctx, ws, "one", "hello", PurposePlayground)
		var f invocationFrame
		if err := wsjson.Read(ctx, ws, &f); err != nil || f.Type != "delta" || f.Text != "hello" {
			t.Errorf("delta: %+v %v", f, err)
		}
		if err := wsjson.Read(ctx, ws, &f); err != nil || f.Type != "completed" {
			t.Errorf("completed: %+v %v", f, err)
		}
		sendInvocation(ctx, ws, "one", "hello", PurposePlayground)
		if err := wsjson.Read(ctx, ws, &f); err != nil || f.Type != "failed" || f.Error != "duplicate invocation" {
			t.Errorf("replay: %+v %v", f, err)
		}
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "revoked"})
		_, _, _ = ws.Read(ctx)
	})
	if !errors.Is(c.Wait(), ErrConnectionRevoked) {
		t.Fatalf("want revoked, got %v", c.Wait())
	}
}
func TestInvocationRejectsPurposeAndCancelsHandler(t *testing.T) {
	_, c := invocationFixture(t, func(ctx context.Context, ws *websocket.Conn) {
		sendInvocation(ctx, ws, "bad", "hello", PurposeSimulation)
		var f invocationFrame
		if err := wsjson.Read(ctx, ws, &f); err != nil || f.Error != "purpose not authorized" {
			t.Errorf("scope: %+v %v", f, err)
		}
		sendInvocation(ctx, ws, "wait", "wait", PurposePlayground)
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "cancel", ID: "wait"})
		if err := wsjson.Read(ctx, ws, &f); err != nil || f.Type != "failed" || f.Error != context.Canceled.Error() {
			t.Errorf("cancel: %+v %v", f, err)
		}
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "revoked"})
		_, _, _ = ws.Read(ctx)
	})
	if !errors.Is(c.Wait(), ErrConnectionRevoked) {
		t.Fatal(c.Wait())
	}
}
func TestGuardCloseStopsInvocationConnection(t *testing.T) {
	g, c := invocationFixture(t, func(ctx context.Context, ws *websocket.Conn) { _, _, _ = ws.Read(ctx) })
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	if c.Wait() == nil {
		t.Fatal("closed connection has no terminal status")
	}
}

func TestRemoteInvocationCannotBypassProtectionDuringBaseline(t *testing.T) {
	g, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	g.simCtl = simulation.New("test")
	g.simCtl.OnConfigChange(&simulation.Config{Active: true, RunID: "run", TelemetryURL: "ws://127.0.0.1:1", TelemetryJWT: "test", ProtectionDisabled: true})
	ctx := context.WithValue(context.Background(), invocationContextKey{}, invocationScope{runID: "run", allowInjection: true})
	dec, err := g.Check(ctx, CheckRequest{Stage: StagePreLLM, Content: Content{Full: "hello"}})
	if err == nil || dec.Reason != "remote_simulation_not_authorized" {
		t.Fatalf("baseline escaped remote scope: %+v %v", dec, err)
	}
}

func TestInvocationLeaseDefaults(t *testing.T) {
	o, err := (InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}}).validate()
	if err != nil || o.LeaseDuration != time.Hour || o.AutoRenew == nil || !*o.AutoRenew {
		t.Fatalf("defaults: %+v %v", o, err)
	}
	finite := false
	o, err = (InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}, AutoRenew: &finite}).validate()
	if err != nil || *o.AutoRenew {
		t.Fatalf("finite: %+v %v", o, err)
	}
}
func TestInvocationRenewsAcrossOriginalExpiryThenRevokes(t *testing.T) {
	started := time.Now()
	_, c := invocationLeaseFixture(t, time.Second, nil, func(ctx context.Context, ws *websocket.Conn) {
		// Hold an active handler across multiple renewals; it must not be canceled by the original expiry.
		sendInvocation(ctx, ws, "long", "wait", PurposePlayground)
		for i := 0; i < 3; i++ {
			var f invocationFrame
			if err := wsjson.Read(ctx, ws, &f); err != nil || f.Type != "renew" || f.ID == "" {
				t.Errorf("renewal %d: %+v %v", i, f, err)
				return
			}
			_ = wsjson.Write(ctx, ws, invocationFrame{Type: "renewed", ID: f.ID, ExpiresAt: time.Now().Add(time.Second)})
		}
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "cancel", ID: "long"})
		var f invocationFrame
		if err := wsjson.Read(ctx, ws, &f); err != nil || f.Type != "failed" || f.Error != context.Canceled.Error() {
			t.Errorf("handler did not survive renewal: %+v %v", f, err)
		}
		sendInvocation(ctx, ws, "after", "hello", PurposePlayground)
		for _, kind := range []string{"delta", "completed"} {
			if err := wsjson.Read(ctx, ws, &f); err != nil || f.Type != kind {
				t.Errorf("post-expiry invocation: %+v %v", f, err)
			}
		}
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "revoked"})
		_, _, _ = ws.Read(ctx)
	})
	if err := c.Wait(); !errors.Is(err, ErrConnectionRevoked) {
		t.Fatal(err)
	}
	if time.Since(started) < time.Second || !c.ExpiresAt().After(started.Add(time.Second)) {
		t.Fatal("lease did not advance")
	}
}
func TestInvocationFiniteLeaseAndMissingAcknowledgementExpire(t *testing.T) {
	for _, auto := range []bool{false, true} {
		t.Run(fmt.Sprint(auto), func(t *testing.T) {
			_, c := invocationLeaseFixture(t, time.Second, &auto, func(ctx context.Context, ws *websocket.Conn) {
				var f invocationFrame
				err := wsjson.Read(ctx, ws, &f)
				if auto {
					if err != nil || f.Type != "renew" {
						t.Errorf("expected renewal: %+v %v", f, err)
					}
					_, _, _ = ws.Read(ctx) // No ack: the SDK must not extend its own authorization.
				} else if err == nil {
					t.Errorf("finite session sent a frame: %+v", f)
				}
			})
			if err := c.Wait(); !errors.Is(err, ErrConnectionExpired) {
				t.Fatalf("expected expiry, got %v", err)
			}
		})
	}
}

func TestClientStateDefaultsEnabledAndPreservesTypes(t *testing.T) {
	disabled, enabled := false, true
	for name, allow := range map[string]*bool{"default": nil, "disabled": &disabled, "enabled": &enabled} {
		t.Run(name, func(t *testing.T) {
			_, c := invocationStateFixture(t, time.Minute, nil, allow, func(ctx context.Context, ws *websocket.Conn) {
				for i, state := range []string{`{}`, `{"form":{"enabled":false,"count":2,"selected":null}}`} {
					request := map[string]any{"id": fmt.Sprint(i), "purpose": "playground", "session_id": "session", "messages": []InvocationMessage{{Role: "user", Content: "hello"}}, "deadline": time.Now().Add(time.Minute), "state": json.RawMessage(state)}
					if err := wsjson.Write(ctx, ws, map[string]any{"type": "invoke", "request": request}); err != nil {
						t.Error(err)
						return
					}
					var f invocationFrame
					if err := wsjson.Read(ctx, ws, &f); err != nil {
						t.Error(err)
						return
					}
					if allow != nil && !*allow {
						if f.Type != "failed" || f.Error != "client state not authorized" {
							t.Errorf("unexpected rejection: %+v", f)
						}
					} else {
						var actual, expected any
						_ = json.Unmarshal([]byte(f.Text), &actual)
						_ = json.Unmarshal([]byte(state), &expected)
						if f.Type != "delta" || !reflect.DeepEqual(actual, expected) {
							t.Errorf("state lost: %+v", f)
						}
						if err := wsjson.Read(ctx, ws, &f); err != nil || f.Type != "completed" {
							t.Errorf("completion: %+v %v", f, err)
						}
					}
				}
				_ = wsjson.Write(ctx, ws, invocationFrame{Type: "revoked"})
				_, _, _ = ws.Read(ctx)
			})
			if !errors.Is(c.Wait(), ErrConnectionRevoked) {
				t.Fatal(c.Wait())
			}
		})
	}
}

func TestInvocationHeartbeatUsesLibraryPingPong(t *testing.T) {
	_, c := invocationFixture(t, func(ctx context.Context, ws *websocket.Conn) {
		// Reading automatically answers control-frame pings without application messages.
		_, _, _ = ws.Read(ctx)
	})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 160*time.Millisecond)
	defer cancel()
	err := invocationHeartbeat(ctx, c.ws, 20*time.Millisecond, 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("heartbeat: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("heartbeat failed before the test ended")
	}
}

func TestInvocationHeartbeatTimesOutWithoutPong(t *testing.T) {
	_, c := invocationFixture(t, func(ctx context.Context, ws *websocket.Conn) {
		// Deliberately do not read, so the library cannot answer pings.
		<-ctx.Done()
	})
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := invocationHeartbeat(ctx, c.ws, 20*time.Millisecond, 40*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("expected pong deadline before parent deadline, got %v", err)
	}
}

func TestInvocationReadErrorPreservesCause(t *testing.T) {
	_, c := invocationFixture(t, func(ctx context.Context, ws *websocket.Conn) {
		_ = ws.Close(websocket.StatusGoingAway, "test shutdown")
	})
	err := c.Wait()
	if !errors.Is(err, ErrInvocationOutcomeUnknown) || websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("lost socket error detail: %v", err)
	}
}

func TestHistoryDeliveryAndRejectionOverReverseChannel(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		_, c := invocationHistoryFixture(t, time.Minute, nil, nil, allowed, func(ctx context.Context, ws *websocket.Conn) {
			request := map[string]any{"id": "history", "purpose": "playground", "session_id": "session", "messages": []InvocationMessage{{Role: "user", Content: "Which city?"}}, "history": []InvocationMessage{{Role: "assistant", Content: "Kyoto"}}, "deadline": time.Now().Add(time.Minute)}
			if err := wsjson.Write(ctx, ws, map[string]any{"type": "invoke", "request": request}); err != nil {
				t.Error(err)
				return
			}
			var frame invocationFrame
			if err := wsjson.Read(ctx, ws, &frame); err != nil {
				t.Error(err)
				return
			}
			if allowed {
				if frame.Type != "delta" || !strings.Contains(frame.Text, "Kyoto") {
					t.Errorf("history lost: %+v", frame)
				}
				if err := wsjson.Read(ctx, ws, &frame); err != nil || frame.Type != "completed" {
					t.Errorf("completion: %+v %v", frame, err)
				}
			} else if frame.Error != "client history not authorized" {
				t.Errorf("not rejected: %+v", frame)
			}
			_ = wsjson.Write(ctx, ws, invocationFrame{Type: "revoked"})
		})
		if err := c.Wait(); !errors.Is(err, ErrConnectionRevoked) {
			t.Fatal(err)
		}
	}
}

func TestConcurrentInvocationsCancelOnlyTheirOwnHandler(t *testing.T) {
	_, c := invocationCapacityFixture(t, time.Minute, nil, nil, false, 2, func(ctx context.Context, ws *websocket.Conn) {
		first := &InvocationRequest{ID: "one", Purpose: PurposePlayground, Messages: []InvocationMessage{{Role: "user", Content: "wait"}}, Deadline: time.Now().Add(time.Minute)}
		first.SessionID = "one-session"
		second := &InvocationRequest{ID: "two", Purpose: PurposePlayground, Messages: []InvocationMessage{{Role: "user", Content: "hello"}}, Deadline: time.Now().Add(time.Minute)}
		second.SessionID = "two-session"
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "invoke", Request: first})
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "invoke", Request: second})
		var frame invocationFrame
		if err := wsjson.Read(ctx, ws, &frame); err != nil || frame.ID != "two" || frame.Type != "delta" {
			t.Errorf("second call blocked: %+v %v", frame, err)
			return
		}
		if err := wsjson.Read(ctx, ws, &frame); err != nil || frame.ID != "two" || frame.Type != "completed" {
			t.Errorf("second completion: %+v %v", frame, err)
			return
		}
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "cancel", ID: "one"})
		if err := wsjson.Read(ctx, ws, &frame); err != nil || frame.ID != "one" || frame.Type != "failed" {
			t.Errorf("first cancellation: %+v %v", frame, err)
		}
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "revoked"})
	})
	if err := c.Wait(); !errors.Is(err, ErrConnectionRevoked) {
		t.Fatal(err)
	}
}

func TestConcurrentInvocationPrivateSnapshots(t *testing.T) {
	g, c := invocationCapacityFixture(t, time.Minute, nil, nil, false, 2, func(ctx context.Context, ws *websocket.Conn) {
		for _, id := range []string{"one", "two"} {
			content := "go"
			if id == "one" {
				content = "wait"
			}
			session := "eval:run:" + id
			plan := json.RawMessage(`{"id":"` + id + `","session_id":"` + session + `","step_id":"s","overrides":[{"id":"r","tool":"read","occurrence":1,"arguments":{},"value":"` + id + `","required_matches":1}]}`)
			req := &InvocationRequest{ID: id, Purpose: PurposePlayground, Messages: []InvocationMessage{{Role: "user", Content: content}}, RunID: "run", SessionID: session, Deadline: time.Now().Add(time.Minute), ScenarioPlan: plan}
			if err := wsjson.Write(ctx, ws, invocationFrame{Type: "invoke", Request: req}); err != nil {
				t.Error(err)
				return
			}
			var f invocationFrame
			if err := wsjson.Read(ctx, ws, &f); err != nil {
				t.Error(err)
				return
			}
			if f.Type != "delta" || f.Text != `"`+id+`"` {
				t.Errorf("unexpected frame %+v", f)
				return
			}
		}
		var f invocationFrame
		if err := wsjson.Read(ctx, ws, &f); err != nil {
			t.Error(err)
			return
		}
		if f.Type != "completed" || f.ID != "two" || f.ScenarioSnapshot["plan_id"] != "two" || f.ScenarioSnapshot["complete"] != true {
			t.Errorf("bad second completion %+v", f)
		}
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "cancel", ID: "one"})
		if err := wsjson.Read(ctx, ws, &f); err != nil {
			t.Error(err)
			return
		}
		if f.Type != "failed" || f.ID != "one" || f.ScenarioSnapshot["plan_id"] != "one" || f.ScenarioSnapshot["handler_success"] != false {
			t.Errorf("bad first completion %+v", f)
		}
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "revoked"})
	}, true)
	defer g.Close()
	if err := c.Wait(); !errors.Is(err, ErrConnectionRevoked) {
		t.Fatal(err)
	}
}

func TestInvocationObservationDoesNotRequireInjection(t *testing.T) {
	for _, raw := range []string{`{"overrides":[]}`, `{"overrides":[{"enabled":false}]}`} {
		if scenarioPlanRequiresInjection(json.RawMessage(raw)) {
			t.Fatal("observation requires injection")
		}
	}
	if !scenarioPlanRequiresInjection(json.RawMessage(`{"overrides":[{}]}`)) {
		t.Fatal("enabled mock requires permission")
	}
}

func TestInvocationInjectionDefaultAndOptOut(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		o := InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposeEval}}
		if !enabled {
			o.AllowSimulationInjection = &enabled
		}
		normalized, err := o.validate()
		if err != nil {
			t.Fatal(err)
		}
		if normalized.AllowSimulationInjection == nil || *normalized.AllowSimulationInjection != enabled {
			t.Fatal("injection default or explicit opt-out lost")
		}
		raw, err := json.Marshal(normalized)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			Enabled bool `json:"allow_simulation_injection"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		if wire.Enabled != enabled {
			t.Fatal("registration permission disagrees with local enforcement")
		}
	}
}
