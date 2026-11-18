// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/compfly-ai/flyedge-go"
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

// TestProxyModeCarriesTurnTraceparent: a proxied call carries the caller's W3C trace, so prism
// records its pre/post-LLM checks under the same turn as the SDK's own checks.
func TestProxyModeCarriesTurnTraceparent(t *testing.T) {
	g, err := flyedge.New(flyedge.Config{APIURL: "https://prism.local", ProxyMode: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cb := &captureBase{}
	client := &http.Client{Transport: g.WrapRoundTripper(cb)}
	const traceID, spanID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`))
	req = req.WithContext(flyedge.ContextWithTrace(req.Context(), traceID, spanID))
	if _, err := client.Do(req); err != nil {
		t.Fatalf("do: %v", err)
	}
	if got, want := cb.req.Header.Get("Traceparent"), "00-"+traceID+"-"+spanID+"-01"; got != want {
		t.Errorf("Traceparent = %q, want %q", got, want)
	}
}

// TestProxyModeTraceparentFollowsSession: without a caller trace, proxied calls in one session
// share a trace id, the same fallback the SDK's checks use.
func TestProxyModeTraceparentFollowsSession(t *testing.T) {
	g, err := flyedge.New(flyedge.Config{APIURL: "https://prism.local", ProxyMode: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cb := &captureBase{}
	client := &http.Client{Transport: g.WrapRoundTripper(cb)}
	traceOf := func() string {
		req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions",
			strings.NewReader(`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`))
		req = req.WithContext(flyedge.ContextWithSession(req.Context(), "sess-trace"))
		if _, err := client.Do(req); err != nil {
			t.Fatalf("do: %v", err)
		}
		parts := strings.Split(cb.req.Header.Get("Traceparent"), "-")
		if len(parts) != 4 || len(parts[1]) != 32 || len(parts[2]) != 16 {
			t.Fatalf("malformed Traceparent %q", cb.req.Header.Get("Traceparent"))
		}
		return parts[1]
	}
	if first, second := traceOf(), traceOf(); first != second {
		t.Errorf("trace ids differ across one session: %q vs %q", first, second)
	}
}
