package flyedge

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Exercise PR52 across the reverse-invocation completion boundary, rather than
// inspecting only the transport accumulator. Neither fixture contacts a cloud.
func TestPR52StreamUsageOnInvocationCompletion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames := make(chan invocationFrame, 1)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": strings.Repeat("a", 32), "token": "fixture", "expires_at": time.Now().Add(time.Minute)})
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.CloseNow()
		if err = wsjson.Write(ctx, ws, invocationFrame{Type: "ready"}); err != nil {
			t.Error(err)
			return
		}
		if err = wsjson.Write(ctx, ws, invocationFrame{Type: "invoke", Request: &InvocationRequest{ID: "pr52-stream", Purpose: PurposePlayground, SessionID: "pr52-session", Messages: []InvocationMessage{{Role: "user", Content: "Say ok"}}, Deadline: time.Now().Add(5 * time.Second)}}); err != nil {
			t.Error(err)
			return
		}
		var frame invocationFrame
		if err = wsjson.Read(ctx, ws, &frame); err != nil {
			t.Error(err)
			return
		}
		frames <- frame
		_ = wsjson.Write(ctx, ws, invocationFrame{Type: "revoked"})
		_, _, _ = ws.Read(ctx)
	}))
	defer broker.Close()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(Config{APIURL: broker.URL, DID: "did:compfly:org:agent", KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}, WithoutCloudTelemetry(), WithEnforcer(usageChecker{}))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	var providerCalls atomic.Int32
	base := usageBase(func(req *http.Request) (*http.Response, error) {
		providerCalls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		opts, _ := body["stream_options"].(map[string]any)
		if opts["include_usage"] != true {
			return nil, errors.New("PR52 did not request stream usage")
		}
		stream := "data: {\"model\":\"gpt-5-mini\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14,\"prompt_tokens_details\":{\"cached_tokens\":3},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	conn, err := g.ServeInvocations(ctx, InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposePlayground}, AllowInsecureLoopback: true}, func(ctx context.Context, _ InvocationRequest, _ InvocationWriter) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/chat/completions", strings.NewReader(`{"model":"gpt-5-mini","stream":true,"messages":[{"role":"user","content":"Say ok"}]}`))
		if err != nil {
			return err
		}
		resp, err := g.WrapRoundTripper(base).RoundTrip(req)
		if err != nil {
			return err
		}
		// EOF and Close both pass through the monitor: accounting must happen once.
		_, readErr := io.Copy(io.Discard, resp.Body)
		closeErr := resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		return closeErr
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Wait(); !errors.Is(err, ErrConnectionRevoked) {
		t.Fatal(err)
	}
	select {
	case frame := <-frames:
		u := frame.AgentUsage
		if frame.Type != "completed" || u == nil {
			t.Fatalf("completion=%+v", frame)
		}
		if providerCalls.Load() != 1 || u.ModelCalls != 1 || u.ReportedCalls != 1 || u.UnknownCalls != 0 {
			t.Fatalf("calls provider=%d usage=%+v", providerCalls.Load(), u)
		}
		if u.InputTokens == nil || *u.InputTokens != 10 || u.OutputTokens == nil || *u.OutputTokens != 4 || u.TotalTokens == nil || *u.TotalTokens != 14 || u.CachedInputTokens == nil || *u.CachedInputTokens != 3 || u.ReasoningTokens == nil || *u.ReasoningTokens != 2 {
			t.Fatalf("tokens=%+v", u)
		}
		if len(u.ByModel) != 1 || u.ByModel[0].TotalTokens == nil || *u.ByModel[0].TotalTokens != 14 {
			t.Fatalf("by model=%+v", u.ByModel)
		}
		t.Logf("provider calls=1; completion model_calls=1 reported_calls=1 unknown_calls=0 input=10 output=4 total=14 cached=3 reasoning=2; model subtotal=14")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
