package flyedge_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	flyedge "github.com/compfly-ai/flyedge-go"
)

// captureBase records the request the wrap forwards, so we can assert proxy-mode rewriting.
type captureBase struct{ req *http.Request }

func (c *captureBase) RoundTrip(r *http.Request) (*http.Response, error) {
	c.req = r
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
}

// TestProxyModeRoutesThroughPrism: with ProxyMode on, an LLM request is rewritten to prism
// /v1/proxy with the original URL in X-Destination and the stage header set. (Signing needs a
// signer; unsigned here just checks routing.)
func TestProxyModeRoutesThroughPrism(t *testing.T) {
	g, err := flyedge.New(flyedge.Config{APIURL: "https://prism.local", ProxyMode: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cb := &captureBase{}
	client := &http.Client{Transport: g.WrapRoundTripper(cb)}

	orig := "https://api.anthropic.com/v1/messages"
	req, _ := http.NewRequest(http.MethodPost, orig,
		strings.NewReader(`{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`))
	if _, err := client.Do(req); err != nil {
		t.Fatalf("do: %v", err)
	}

	if cb.req.URL.String() != "https://prism.local/v1/proxy" {
		t.Errorf("routed to %q, want prism /v1/proxy", cb.req.URL.String())
	}
	if got := cb.req.Header.Get("X-Destination"); got != orig {
		t.Errorf("X-Destination = %q, want %q", got, orig)
	}
	if cb.req.Header.Get("X-CompFly-Stage") != "pre_llm" {
		t.Errorf("X-CompFly-Stage = %q", cb.req.Header.Get("X-CompFly-Stage"))
	}
	if cb.req.Header.Get("X-Session-ID") == "" {
		t.Error("X-Session-ID missing: prism would mint a fresh session per request")
	}
}

// TestProxyModeSessionHeader: the rewritten request carries the session id — a per-request one
// from ContextWithSession when set, else the wrap's own session — and an explicit caller-set
// header is left untouched.
func TestProxyModeSessionHeader(t *testing.T) {
	g, err := flyedge.New(flyedge.Config{APIURL: "https://prism.local", ProxyMode: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cb := &captureBase{}
	client := &http.Client{Transport: g.WrapRoundTripper(cb)}
	orig := "https://api.anthropic.com/v1/messages"
	payload := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`

	req, _ := http.NewRequest(http.MethodPost, orig, strings.NewReader(payload))
	req = req.WithContext(flyedge.ContextWithSession(req.Context(), "sess-explicit"))
	if _, err := client.Do(req); err != nil {
		t.Fatalf("do: %v", err)
	}
	if got := cb.req.Header.Get("X-Session-ID"); got != "sess-explicit" {
		t.Errorf("X-Session-ID = %q, want the context session %q", got, "sess-explicit")
	}

	req, _ = http.NewRequest(http.MethodPost, orig, strings.NewReader(payload))
	req.Header.Set("X-Session-ID", "caller-set")
	if _, err := client.Do(req); err != nil {
		t.Fatalf("do: %v", err)
	}
	if got := cb.req.Header.Get("X-Session-ID"); got != "caller-set" {
		t.Errorf("X-Session-ID = %q, want caller-set header preserved", got)
	}
}
