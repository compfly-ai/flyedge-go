package flyedge

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"net/http"
	"net/http/httptest"
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
	_, err = g.ServeInvocations(context.Background(), InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}, Lifetime: time.Minute}, func(context.Context, InvocationRequest, InvocationWriter) error { return nil })
	if err == nil {
		t.Fatal("unsigned guard must not expose an invocation handler")
	}
}

func TestInvocationOptionsRejectImplicitScope(t *testing.T) {
	for _, opts := range []InvocationOptions{
		{},
		{Environment: "dev", Lifetime: time.Minute},
		{Environment: "dev", Purposes: []InvocationPurpose{"unknown"}, Lifetime: time.Minute},
		{Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}, Lifetime: 25 * time.Hour},
	} {
		if _, err := opts.validate(); err == nil {
			t.Fatalf("accepted invalid scope: %+v", opts)
		}
	}
}

func invocationFixture(t *testing.T, peer func(context.Context, *websocket.Conn)) (*Guard, *InvocationConnection) {
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
			_ = json.NewEncoder(w).Encode(map[string]any{"id": strings.Repeat("a", 32), "token": "test-token", "expires_at": time.Now().Add(time.Minute)})
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
	c, err := g.ServeInvocations(ctx, InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}, Lifetime: time.Minute, AllowInsecureLoopback: true}, func(ctx context.Context, r InvocationRequest, w InvocationWriter) error {
		if r.SessionID != "session" || sessionFromContext(ctx) != "session" {
			return errors.New("missing session context")
		}
		if r.Messages[0].Content == "wait" {
			<-ctx.Done()
			return ctx.Err()
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
