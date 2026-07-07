package flyedge_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"os"
	"testing"
	"time"

	flyedge "github.com/compfly-ai/flyedge-go"
)

// TestLiveCheck is the M1 wire-compat checkpoint: build a Guard from a real MCP-minted DID+key and
// call prism /v1/flyedge/check. It proves prism ACCEPTS our Go signature (no 401) and returns a
// decision. Env-gated so it never runs in normal CI.
//
//	FLYEDGE_LIVE=1 \
//	COMPFLY_API_URL=http://localhost:8080 \
//	COMPFLY_AGENT_DID=did:compfly:66f100:1fbad81e2c302b7b69b936e44e0f5c9e \
//	COMPFLY_AGENT_PRIVATE_KEY_PATH=$HOME/flyedge-local-demo/agent_key.pem \
//	go test -run TestLiveCheck -v
func TestLiveCheck(t *testing.T) {
	if os.Getenv("FLYEDGE_LIVE") == "" {
		t.Skip("set FLYEDGE_LIVE=1 (+ COMPFLY_* + running stack) to run the live wire-compat check")
	}
	cfg := flyedge.LoadEnv()
	cfg.Timeout = 15 * time.Second
	g, err := flyedge.New(cfg, flyedge.WithMode(flyedge.ModeEnforce))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Close()
	t.Logf("guard DID=%s", g.DID())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	benign := flyedge.CheckRequest{
		RequestID: "go-live-benign",
		SessionID: "go-live",
		Stage:     flyedge.StagePreLLM,
		Content:   flyedge.Content{Preview: "what are your hours?", Full: "what are your hours?", SizeBytes: 20},
		Operation: flyedge.Operation{Type: "chat.completions", ModelID: "gpt-4o", DestDomain: "api.openai.com"},
	}
	dec, err := g.Check(ctx, benign)
	// The checkpoint is about SIGNATURE ACCEPTANCE: a 401/invalid-signature would surface as an
	// enforcement error (and, fail-open, an Allow with Reason=fail_open + the error message).
	if dec.Reason == "fail_open" {
		t.Fatalf("enforcement call failed (signature rejected or prism unreachable?): %s", dec.Message)
	}
	if err != nil {
		if _, ok := flyedge.AsDenyError(err); !ok {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	t.Logf("benign → action=%s reason=%s policy_version=%s request_id=%s",
		dec.Action, dec.Reason, dec.PolicyVersion, dec.RequestID)
	t.Logf("SIGNATURE ACCEPTED by prism — Go signing is wire-compatible")
}

// TestLiveLifecycle verifies the cloud lifecycle POSTs (connect + telemetry) are accepted by prism.
// Env-gated like TestLiveCheck.
func TestLiveLifecycle(t *testing.T) {
	if os.Getenv("FLYEDGE_LIVE") == "" {
		t.Skip("set FLYEDGE_LIVE=1 (+ COMPFLY_* + running stack)")
	}
	cfg := flyedge.LoadEnv()
	cfg.Timeout = 15 * time.Second
	g, err := flyedge.New(cfg, flyedge.WithCloudTelemetry(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// connect: register a manifest (presence + seeding)
	if err := g.Connect(ctx, flyedge.ManifestInfo{
		Framework: "flyedge-go-test", Tools: []string{"fetch_url"}, Models: []string{"claude-haiku-4-5"}, Environment: "dev",
	}); err != nil {
		t.Fatalf("Connect (prism rejected the manifest?): %v", err)
	}
	t.Log("CONNECT accepted by prism")

	// a check produces a telemetry event; Close flushes it to /v1/flyedge/telemetry
	_, _ = g.Check(ctx, flyedge.CheckRequest{SessionID: "lifecycle", Stage: flyedge.StagePreLLM,
		Content: flyedge.Content{Full: "hello"}, Operation: flyedge.Operation{Type: "chat.completions", ModelID: "claude-haiku-4-5"}})
	if err := g.Close(); err != nil {
		t.Fatalf("Close/telemetry flush (prism rejected telemetry?): %v", err)
	}
	t.Log("TELEMETRY batch flushed + accepted by prism")
}

// TestLiveProxyMode routes an Anthropic call through prism /v1/proxy (in-band). Env-gated.
func TestLiveProxyMode(t *testing.T) {
	if os.Getenv("FLYEDGE_LIVE") == "" {
		t.Skip("set FLYEDGE_LIVE=1")
	}
	cfg := flyedge.LoadEnv()
	cfg.ProxyMode = true
	cfg.Timeout = 20 * time.Second
	g, err := flyedge.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Close()
	hc := &http.Client{Transport: g.WrapRoundTripper(http.DefaultTransport), Timeout: 25 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		strings.NewReader(`{"model":"claude-haiku-4-5","max_tokens":64,"messages":[{"role":"user","content":"say hi in 3 words"}]}`))
	req.Header.Set("x-api-key", os.Getenv("ANTHROPIC_API_KEY"))
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("proxy do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	t.Logf("in-band proxy → HTTP %d: %.200s", resp.StatusCode, string(b))
}
