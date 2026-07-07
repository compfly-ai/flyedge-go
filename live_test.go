package flyedge_test

import (
	"context"
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
