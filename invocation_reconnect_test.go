// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CompFly AI

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
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestOperatorReconnectUsesScopedHeartbeatAndFreshLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	var signal atomic.Bool
	var registrations atomic.Int32
	const requestID = "0123456789abcdef0123456789abcdef"
	firstID, nextID := strings.Repeat("a", 32), strings.Repeat("b", 32)
	revoke := make(chan struct{})
	second := make(chan struct{}, 1)
	completed := make(chan struct{}, 1)
	drop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case configPath:
			if got := r.Header.Get("X-Invocation-Reconnect-Environments"); got != "dev" {
				t.Errorf("heartbeat environment = %q", got)
			}
			if r.Header.Get("X-CompFly-Signature") == "" {
				t.Error("unsigned config heartbeat")
			}
			response := map[string]any{}
			if signal.Load() {
				response["model_mode"] = "gateway"
				response["invocation_reconnect_requests"] = map[string]string{"dev": requestID, "prod": "wrong-scope"}
			}
			_ = json.NewEncoder(w).Encode(response)
		case "/v1/flyedge/invocations":
			var registration struct {
				Environment string              `json:"environment"`
				Purposes    []InvocationPurpose `json:"purposes"`
				RequestID   string              `json:"reconnect_request_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&registration); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.Header.Get("X-CompFly-Signature") == "" || registration.Environment != "dev" ||
				!reflect.DeepEqual(registration.Purposes, []InvocationPurpose{PurposePlayground}) {
				t.Error("reconnect changed serving scope or skipped DID signing")
			}
			id, expected := firstID, ""
			if n := registrations.Add(1); n > 1 {
				id, expected = nextID, requestID
				if n != 2 {
					t.Error("replayed operator request")
					w.WriteHeader(http.StatusGone)
					return
				}
			}
			if registration.RequestID != expected {
				t.Errorf("reconnect request ID = %q, want %q", registration.RequestID, expected)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "token": id, "expires_at": time.Now().Add(time.Minute)})
		default:
			id := firstID
			if strings.HasSuffix(r.URL.Path, "/"+nextID+"/socket") {
				id = nextID
			}
			if r.Header.Get("Authorization") != "Bearer "+id {
				t.Error("upgrade did not use the fresh lease token")
			}
			ws, err := websocket.Accept(w, r, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer ws.CloseNow()
			if err := wsjson.Write(ctx, ws, invocationFrame{Type: "ready"}); err != nil {
				t.Error(err)
				return
			}
			if id == firstID {
				select {
				case <-revoke:
					_ = wsjson.Write(ctx, ws, invocationFrame{Type: "revoked"})
				case <-ctx.Done():
				}
				return
			}
			second <- struct{}{}
			req := InvocationRequest{ID: "fresh", Purpose: PurposePlayground, SessionID: "test", Deadline: time.Now().Add(time.Second)}
			if err := wsjson.Write(ctx, ws, invocationFrame{Type: "invoke", Request: &req}); err != nil {
				t.Error(err)
				return
			}
			var result invocationFrame
			if err := wsjson.Read(ctx, ws, &result); err != nil || result.Type != "completed" {
				t.Errorf("replacement handler result = %+v, err = %v", result, err)
				return
			}
			completed <- struct{}{}
			select {
			case <-drop:
			case <-ctx.Done():
			}
		}
	}))
	defer server.Close()
	guard, err := New(Config{APIURL: server.URL, DID: "did:compfly:test:fixture", KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}, WithHeartbeat(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	options := InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}, LeaseDuration: time.Minute, AllowInsecureLoopback: true, AllowOperatorReconnect: true}
	conn, err := guard.ServeInvocations(ctx, options, func(context.Context, InvocationRequest, InvocationWriter) error {
		if guard.ModelMode() != ModelModeGateway {
			return errors.New("reconnected before applying heartbeat settings")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	close(revoke)
	waitForActiveLease(t, ctx, conn, "")
	if registrations.Load() != 1 {
		t.Fatal("revocation triggered automatic reconnect")
	}
	signal.Store(true)
	for _, event := range []<-chan struct{}{second, completed} {
		select {
		case <-event:
		case <-ctx.Done():
			t.Fatal("operator reconnect did not restore serving")
		}
	}
	if conn.ID != firstID || conn.ActiveLeaseID() != nextID {
		t.Fatal("serving handle or replacement lease has the wrong ID")
	}
	close(drop)
	waitForActiveLease(t, ctx, conn, "")
	if err := conn.Close(); err != nil || !errors.Is(conn.Wait(), context.Canceled) {
		t.Fatal("explicit close did not end the serving session")
	}
	if conn.ActiveLeaseID() != "" || registrations.Load() != 2 {
		t.Fatal("a closed session retained an active lease or registered again")
	}
}

func waitForActiveLease(t *testing.T, ctx context.Context, conn *InvocationConnection, want string) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for conn.ActiveLeaseID() != want {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("active lease = %q, want %q", conn.ActiveLeaseID(), want)
		}
	}
}
