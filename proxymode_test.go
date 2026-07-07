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
}
