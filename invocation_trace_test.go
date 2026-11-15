// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/compfly-ai/flyedge-go/enforce"
)

type invocationTraceEnforcer func(context.Context, enforce.CheckRequest) (enforce.Decision, error)

func (f invocationTraceEnforcer) Check(ctx context.Context, r enforce.CheckRequest) (enforce.Decision, error) {
	return f(ctx, r)
}

func TestInvocationTraceContext(t *testing.T) {
	const first = "00-0123456789abcdef0123456789abcdef-fedcba9876543210-01"
	const second = "00-abcdef0123456789abcdef0123456789-0123456789abcdef-00"
	cases := []string{
		first, second, "", "malformed",
		strings.ToUpper(first), "01" + first[2:],
		"00-00000000000000000000000000000000-fedcba9876543210-01",
		"00-0123456789abcdef0123456789abcdef-0000000000000000-01",
		first + "-extra", first[:len(first)-1],
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The final pair must overlap; otherwise a shared mutable trace could pass.
	var entered sync.WaitGroup
	entered.Add(2)
	gate := make(chan struct{})
	go func() { entered.Wait(); close(gate) }()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": strings.Repeat("a", 32), "token": "token", "expires_at": time.Now().Add(time.Minute)})
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.CloseNow()
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "ready"})
		send := func(id, session, trace string) {
			// Use wire JSON so the regression fails behaviorally before the SDK field exists.
			req := map[string]any{"id": id, "session_id": session, "purpose": PurposePlayground, "messages": []InvocationMessage{{Role: "user", Content: trace}}, "deadline": time.Now().Add(time.Minute)}
			if trace != "" {
				req["traceparent"] = trace
			}
			if err := wsjson.Write(ctx, ws, map[string]any{"type": "invoke", "request": req}); err != nil {
				t.Error(err)
			}
		}
		complete := func() {
			var f invocationFrame
			if err := wsjson.Read(ctx, ws, &f); err != nil || f.Type != "completed" {
				t.Errorf("completion: %+v, %v", f, err)
			}
		}
		for _, tc := range cases {
			send(tc+"id", "same-session", tc)
			complete()
		}
		send("parallel-one", "session-one", first)
		send("parallel-two", "session-two", second)
		complete()
		complete()
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "revoked"})
	}))
	defer server.Close()
	_, priv, _ := ed25519.GenerateKey(nil)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	checker := invocationTraceEnforcer(func(ctx context.Context, r enforce.CheckRequest) (enforce.Decision, error) {
		got := enforce.IdentityHeaders(ctx)["traceparent"]
		supplied := r.Operation.ToolName
		valid := supplied == first || supplied == second
		parts := strings.Split(got, "-")
		if len(parts) != 4 {
			t.Errorf("invalid outgoing traceparent %q", got)
		} else if valid {
			want := strings.Split(supplied, "-")
			if parts[1] != want[1] || parts[2] != want[2] {
				t.Errorf("stage %s: traceparent = %s, want trace/parent from %s", r.Stage, got, supplied)
			}
		} else if parts[1] != deriveTraceID(r.SessionID) {
			t.Errorf("invalid/missing trace %q leaked trace %s", supplied, got)
		}
		if sessionFromContext(ctx) != r.SessionID {
			t.Errorf("session changed: %q != %q", sessionFromContext(ctx), r.SessionID)
		}
		return enforce.Decision{Action: ActionAllow}, nil
	})
	g, err := New(Config{APIURL: server.URL, DID: "did:compfly:org:agent", KeyPEM: key}, WithEnforcer(checker))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	c, err := g.ServeInvocations(ctx, InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}, MaxConcurrentInvocations: 2, AllowInsecureLoopback: true}, func(ctx context.Context, r InvocationRequest, _ InvocationWriter) error {
		if strings.HasPrefix(r.ID, "parallel-") {
			entered.Done()
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		for _, stage := range []Stage{StagePreLLM, StageToolCall, StageToolCallResponse, StagePostLLM} {
			_, err := g.Check(ctx, CheckRequest{SessionID: r.SessionID, Stage: stage, Operation: enforce.Operation{ToolName: r.Messages[0].Content}})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err != ErrConnectionRevoked {
		t.Fatalf("Wait: %v", err)
	}
}
