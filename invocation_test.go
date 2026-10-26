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
	return invocationStateFixture(t, duration, autoRenew, false, peer)
}
func invocationStateFixture(t *testing.T, duration time.Duration, autoRenew *bool, allowState bool, peer func(context.Context, *websocket.Conn)) (*Guard, *InvocationConnection) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	_, priv, _ := ed25519.GenerateKey(nil)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
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
	c, err := g.ServeInvocations(ctx, InvocationOptions{AllowClientState: allowState, Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}, LeaseDuration: duration, AutoRenew: autoRenew, AllowInsecureLoopback: true}, func(ctx context.Context, r InvocationRequest, w InvocationWriter) error {
		if r.SessionID != "session" || sessionFromContext(ctx) != "session" {
			return errors.New("missing session context")
		}
		if r.Messages[0].Content == "wait" {
			<-ctx.Done()
			return ctx.Err()
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

func TestClientStateRequiresOptInAndPreservesTypes(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(fmt.Sprint(allow), func(t *testing.T) {
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
					if !allow {
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
