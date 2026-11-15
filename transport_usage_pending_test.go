package flyedge

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestInvocationUsageOutstandingStreamCannotDisappearBehindKnownCall(t *testing.T) {
	g, err := New(Config{}, WithoutCloudTelemetry(), WithEnforcer(usageChecker{}))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ctx, usage := usageContext(g, "turn", "session")
	calls := 0
	base := usageBase(func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n"))}, nil
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))}, nil
	})
	wrapped := g.WrapRoundTripper(base)
	request := func() *http.Request {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	pending, err := wrapped.RoundTrip(request())
	if err != nil {
		t.Fatal(err)
	}
	known, err := wrapped.RoundTrip(request())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, known.Body); err != nil {
		t.Fatal(err)
	}
	if err := known.Body.Close(); err != nil {
		t.Fatal(err)
	}
	got := usage.finish()
	if got == nil || got.ModelCalls != 2 || got.UnknownCalls != 1 || got.ReportedCalls != 1 || got.TotalTokens != nil || *got.InputTokens != 3 || *got.OutputTokens != 4 {
		t.Fatalf("unclosed started stream disappeared: %+v", got)
	}
	if _, err := io.Copy(io.Discard, pending.Body); err != nil {
		t.Fatal(err)
	}
	if err := pending.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if got.ModelCalls != 2 || got.UnknownCalls != 1 || got.TotalTokens != nil || usage.finish() != nil {
		t.Fatal("late stream completion changed the invocation")
	}
}
