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
	"sync/atomic"
	"testing"
	"time"
)

func TestRecoveryClassificationPreservesAuthorization(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity} {
		if retryInvocationRecovery(&InvocationHTTPError{StatusCode: code}) {
			t.Fatalf("retrying terminal HTTP %d", code)
		}
	}
	for _, code := range []int{409, 429, 500, 503} {
		if !retryInvocationRecovery(&InvocationHTTPError{StatusCode: code}) {
			t.Fatalf("not retrying transient HTTP %d", code)
		}
	}
	for _, err := range []error{ErrConnectionRevoked, ErrConnectionExpired, context.Canceled, errors.New("malformed response")} {
		if retryInvocationRecovery(err) {
			t.Fatalf("retrying terminal error %v", err)
		}
	}
}
func TestRecoveryBackoffIsBounded(t *testing.T) {
	for attempt := uint64(1); attempt < 100; attempt++ {
		d := invocationRecoveryDelay(attempt)
		if d < 500*time.Millisecond || d > 30*time.Second {
			t.Fatalf("invalid backoff %v", d)
		}
	}
}

func TestRecoverySurvivesPendingHandshakeAndKeepsSessionAlive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_, key, _ := ed25519.GenerateKey(nil)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	id := strings.Repeat("a", 32)
	expiry := time.Now().Add(time.Minute)
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.URL.Path != "/v1/flyedge/invocations/"+id+"/recover" {
				t.Error("recovery attempted ordinary registration")
				w.WriteHeader(400)
				return
			}
			if r.Header.Get("X-CompFly-Signature") == "" {
				t.Error("unsigned recovery")
			}
			if attempts.Add(1) == 1 {
				w.WriteHeader(409)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": id, "token": "rotated", "expires_at": expiry, "recovery_protocol_version": 1})
			return
		}
		if r.Header.Get("Authorization") != "Bearer rotated" {
			t.Error("missing rotated token")
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		wsjson.Write(ctx, ws, invocationFrame{Type: "ready"})
		ws.Read(ctx)
	}))
	defer server.Close()
	g, err := New(Config{APIURL: server.URL, DID: "did:compfly:org:agent", KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	c := &InvocationConnection{}
	lease, err := c.recover(ctx, g, InvocationOptions{AutoReconnect: true, AllowInsecureLoopback: true, LeaseDuration: time.Minute}, &invocationLease{id: id, expiresAt: expiry})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.cancel()
	defer lease.ws.CloseNow()
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
	if err := lease.ctx.Err(); err != nil {
		t.Fatalf("recovered session was canceled on return: %v", err)
	}
	lease.ws.CloseRead(lease.ctx)
	if err := lease.ws.Ping(lease.ctx); err != nil {
		t.Fatalf("recovered socket unusable: %v", err)
	}
}

func TestRecoveryStopsBeforeAttemptWhenCanceledOrExpired(t *testing.T) {
	for _, expired := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		expiry := time.Now().Add(time.Minute)
		want := error(context.Canceled)
		if expired {
			expiry = time.Now().Add(-time.Second)
			want = ErrConnectionExpired
		} else {
			cancel()
		}
		c := &InvocationConnection{}
		_, err := c.recover(ctx, nil, InvocationOptions{}, &invocationLease{expiresAt: expiry})
		cancel()
		if !errors.Is(err, want) {
			t.Fatalf("error=%v, want %v", err, want)
		}
		if c.recoveryAttempts != 0 {
			t.Fatal("attempted recovery after cancellation/expiry")
		}
	}
}
