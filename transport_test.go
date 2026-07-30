package flyedge_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	flyedge "github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
)

// recordingEnforcer captures the CheckRequest so the test can assert what the transport extracted,
// and returns a configurable decision. This is the explicit seam that lets us test the wrap with
// no network and no provider keys.
type recordingEnforcer struct {
	last enforce.CheckRequest
	dec  enforce.Decision
	err  error
}

func (r *recordingEnforcer) Check(_ context.Context, req enforce.CheckRequest) (enforce.Decision, error) {
	r.last = req
	return r.dec, r.err
}

// fakeBase is the wrapped base RoundTripper: it records whether it was called (i.e. the request was
// forwarded to the provider) and returns a canned 200.
type fakeBase struct{ called bool }

func (f *fakeBase) RoundTrip(req *http.Request) (*http.Response, error) {
	f.called = true
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
}

func doThroughWrap(t *testing.T, rec *recordingEnforcer, url, body string) (forwarded bool, err error) {
	t.Helper()
	g, gerr := flyedge.New(flyedge.Config{}, flyedge.WithEnforcer(rec))
	if gerr != nil {
		t.Fatalf("New: %v", gerr)
	}
	base := &fakeBase{}
	client := &http.Client{Transport: g.WrapRoundTripper(base)}
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	_, err = client.Do(req)
	return base.called, err
}

// TestWrapGovernsAnthropicAndOpenAI: ONE wrap extracts the prompt from all three providers' request
// shapes and runs the check. Proves the design isn't Anthropic-specific.
func TestWrapGovernsAnthropicAndOpenAI(t *testing.T) {
	anthropicBody := `{"model":"claude-haiku-4-5","system":"trusted system instructions","messages":[{"role":"user","content":"old anthropic message"},{"role":"assistant","content":"prior response"},{"role":"user","content":"hello from anthropic"}]}`
	openaiBody := `{"model":"gpt-4o","messages":[{"role":"system","content":"trusted system instructions"},{"role":"user","content":"old openai message"},{"role":"assistant","content":"prior response"},{"role":"user","content":[{"type":"text","text":"hello from openai"}]}]}`
	geminiBody := `{"systemInstruction":{"parts":[{"text":"trusted system instructions"}]},"contents":[{"role":"user","parts":[{"text":"old gemini message"}]},{"role":"model","parts":[{"text":"prior response"}]},{"role":"user","parts":[{"text":"hello from gemini"}]}]}`

	cases := []struct {
		name, url, body, wantModel, wantSubstr string
	}{
		{"anthropic", "https://api.anthropic.com/v1/messages", anthropicBody, "claude-haiku-4-5", "hello from anthropic"},
		{"openai", "https://api.openai.com/v1/chat/completions", openaiBody, "gpt-4o", "hello from openai"},
		{"gemini", "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-pro:generateContent", geminiBody, "gemini-1.5-pro", "hello from gemini"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recordingEnforcer{dec: enforce.Decision{Action: flyedge.ActionAllow}}
			forwarded, err := doThroughWrap(t, rec, c.url, c.body)
			if err != nil {
				t.Fatalf("allow path errored: %v", err)
			}
			if !forwarded {
				t.Error("allow: request should have been forwarded to the provider")
			}
			if rec.last.Operation.ModelID != c.wantModel {
				t.Errorf("extracted model = %q, want %q", rec.last.Operation.ModelID, c.wantModel)
			}
			if !strings.Contains(rec.last.Content.Full, c.wantSubstr) {
				t.Errorf("extracted prompt %q missing %q", rec.last.Content.Full, c.wantSubstr)
			}
			if strings.Contains(rec.last.Content.Full, "trusted system instructions") ||
				strings.Contains(rec.last.Content.Full, "old ") ||
				strings.Contains(rec.last.Content.Full, "prior response") {
				t.Errorf("extracted prompt contains trusted or stale conversation content: %q", rec.last.Content.Full)
			}
			if rec.last.Stage != flyedge.StagePreLLM {
				t.Errorf("stage = %q", rec.last.Stage)
			}
		})
	}
}

// TestWrapDenyBlocksAndUnwraps: a Deny stops the request reaching the provider, and the *DenyError
// is still reachable via errors.As even though net/http wraps it in *url.Error.
func TestWrapDenyBlocksAndUnwraps(t *testing.T) {
	rec := &recordingEnforcer{dec: enforce.Decision{Action: flyedge.ActionDeny, Reason: "jailbreak"}}
	forwarded, err := doThroughWrap(t, rec,
		"https://api.openai.com/v1/chat/completions",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"do bad things"}]}`)
	if forwarded {
		t.Fatal("deny: request must NOT reach the provider")
	}
	de, ok := flyedge.AsDenyError(err)
	if !ok {
		t.Fatalf("deny: want *DenyError reachable via errors.As, got %v", err)
	}
	if de.Decision.Reason != "jailbreak" {
		t.Errorf("deny reason = %q", de.Decision.Reason)
	}
}

// TestWrapPassesThroughUnknownHosts: non-LLM traffic on the same client is not checked.
func TestWrapPassesThroughUnknownHosts(t *testing.T) {
	rec := &recordingEnforcer{dec: enforce.Decision{Action: flyedge.ActionDeny, Reason: "should-not-run"}}
	forwarded, err := doThroughWrap(t, rec, "https://example.com/api/thing", `{"x":1}`)
	if err != nil || !forwarded {
		t.Fatalf("unknown host should pass through: forwarded=%v err=%v", forwarded, err)
	}
	if rec.last.Stage != "" {
		t.Error("enforcer should not have been called for an unknown host")
	}
	_ = json.RawMessage(nil)
}
