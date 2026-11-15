package flyedge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/compfly-ai/flyedge-go/enforce"
)

type usageBase func(*http.Request) (*http.Response, error)

func (f usageBase) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type usageChecker struct{ deny bool }

func (c usageChecker) Check(context.Context, enforce.CheckRequest) (enforce.Decision, error) {
	if c.deny {
		return enforce.Decision{Action: ActionDeny}, nil
	}
	return enforce.Decision{Action: ActionAllow}, nil
}

func TestInvocationUsageWrappedProviderFormats(t *testing.T) {
	for _, tc := range []struct {
		name, url, body, response string
		input, total              uint64
	}{
		{"openai", "https://api.openai.com/v1/chat/completions", `{"model":"model","messages":[{"role":"user","content":"hi"}]}`, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`, 10, 14},
		{"anthropic", "https://api.anthropic.com/v1/messages", `{"model":"model","messages":[{"role":"user","content":"hi"}]}`, `{"content":[{"text":"ok"}],"usage":{"input_tokens":7,"cache_read_input_tokens":3,"cache_creation_input_tokens":1,"output_tokens":4}}`, 11, 15},
		{"gemini", "https://generativelanguage.googleapis.com/v1beta/models/model:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":14,"cachedContentTokenCount":3}}`, 10, 14},
		{"responses", "https://api.openai.com/v1/responses", `{"model":"model","input":"hi"}`, `{"status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":2}}}`, 10, 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, err := New(Config{}, WithoutCloudTelemetry(), WithEnforcer(usageChecker{}))
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			ctx, usage := usageContext(g, "turn", "session")
			base := usageBase(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.response))}, nil
			})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, tc.url, strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			resp, err := g.WrapRoundTripper(base).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if string(raw) != tc.response {
				t.Fatal("provider body changed")
			}
			got := usage.finish()
			if got == nil || got.ModelCalls != 1 || got.UnknownCalls != 0 || *got.InputTokens != tc.input || *got.OutputTokens != 4 || *got.TotalTokens != tc.total || *got.CachedInputTokens != 3 {
				t.Fatalf("usage=%+v", got)
			}
		})
	}
}

type brokenUsageBody struct{ io.Reader }

func (b brokenUsageBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		return n, errors.New("broken stream")
	}
	return n, err
}
func (brokenUsageBody) Close() error { return nil }

func TestInvocationUsageTransportFailuresAndStreamsStayConservative(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\ndata: [DONE]\n\n"
	for _, name := range []string{"complete", "early-close", "read-error", "missing-terminal", "http-error", "network-error", "panic", "cancelled", "missing-usage", "measured-zero"} {
		t.Run(name, func(t *testing.T) {
			g, err := New(Config{}, WithoutCloudTelemetry(), WithEnforcer(usageChecker{}))
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			ctx, usage := usageContext(g, "turn", "session")
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			base := usageBase(func(*http.Request) (*http.Response, error) {
				if name == "network-error" {
					return nil, errors.New("network failed")
				}
				if name == "panic" {
					panic("provider panic")
				}
				if name == "cancelled" {
					cancel()
				}
				body := stream
				if name == "missing-terminal" {
					body = strings.Split(stream, "data: [DONE]")[0]
				}
				if name == "missing-usage" {
					body = "data: {\"choices\":[]}\n\ndata: [DONE]\n\n"
				}
				if name == "measured-zero" {
					body = "data: {\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n"
				}
				rc := io.ReadCloser(io.NopCloser(strings.NewReader(body)))
				if name == "read-error" {
					rc = brokenUsageBody{strings.NewReader(body)}
				}
				status := 200
				if name == "http-error" {
					status = 500
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: rc}, nil
			})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hi"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if value := recover(); value != nil && name != "panic" {
						panic(value)
					}
				}()
				resp, err := g.WrapRoundTripper(base).RoundTrip(req)
				if err != nil {
					if name != "network-error" {
						t.Error(err)
					}
					return
				}
				if resp == nil {
					t.Fatal("missing response")
				}
				if name == "early-close" {
					_, err = resp.Body.Read(make([]byte, 4096))
				} else {
					_, err = io.Copy(io.Discard, resp.Body)
				}
				if err != nil && name != "read-error" {
					t.Error(err)
				}
				if err := resp.Body.Close(); err != nil {
					t.Error(err)
				}
				if err := resp.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			got := usage.finish()
			if got == nil || got.ModelCalls != 1 {
				t.Fatalf("started attempt missing/duplicated: %+v", got)
			}
			if name == "complete" || name == "measured-zero" {
				want := uint64(14)
				if name == "measured-zero" {
					want = 0
				}
				if got.UnknownCalls != 0 || got.TotalTokens == nil || *got.TotalTokens != want {
					t.Fatalf("measured stream changed: %+v", got)
				}
			} else if got.UnknownCalls != 1 || got.TotalTokens != nil {
				t.Fatalf("incomplete attempt reported complete: %+v", got)
			}
		})
	}
}

func TestInvocationUsageDeniedRequestsNeverStartModelAttempts(t *testing.T) {
	g, err := New(Config{}, WithoutCloudTelemetry(), WithEnforcer(usageChecker{deny: true}))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ctx, usage := usageContext(g, "turn", "session")
	called := false
	base := usageBase(func(*http.Request) (*http.Response, error) { called = true; return nil, errors.New("must not execute") })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.openai.com/v1/chat/completions", strings.NewReader(`{"model":"model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.WrapRoundTripper(base).RoundTrip(req); err == nil || called {
		t.Fatal("pre-denied model executed")
	}
	if usage.finish() != nil {
		t.Fatal("pre-denied call counted as model attempt")
	}
}

func TestInvocationUsageProtocolCountersRequireMeasuredConsistentValues(t *testing.T) {
	for _, raw := range []string{
		`{"usage":{"prompt_tokens":-1,"completion_tokens":4}}`,
		`{"usage":{"prompt_tokens":true,"completion_tokens":4}}`,
		`{"usage":{"prompt_tokens":1.5,"completion_tokens":4}}`,
		`{"usage":{"prompt_tokens":9007199254740992,"completion_tokens":4}}`,
		`{"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":3}}`,
		`{"usage":{"prompt_tokens":10,"completion_tokens":4}} trailing`,
		`{"usage":{"prompt_tokens":10,"completion_tokens":4}} {}`,
	} {
		counts, _ := observedModelUsage([]byte(raw), false)
		if counts != nil {
			t.Fatalf("invalid response became measured usage: %s", raw)
		}
	}
	for _, raw := range []string{
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7,\"cache_read_input_tokens\":3}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\ndata: {\"type\":\"message_stop\"}\n\n",
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":4}}}\n\n",
		"data: {\"type\":\"response.created\",\"response\":{\"status\":\"in_progress\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":4}}}\n\n",
		"data: {\"candidates\":[{\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":4}}\n\n",
	} {
		counts, partial := observedModelUsage([]byte(raw), true)
		if partial || counts == nil || *counts.InputTokens != 10 || *counts.OutputTokens != 4 {
			t.Fatalf("terminal measured protocol lost usage: %+v partial=%v", counts, partial)
		}
	}
}

func TestInvocationUsageProxyRetainsOriginalModelAndDoesNotDoubleCount(t *testing.T) {
	g, err := New(Config{ProxyMode: true, APIURL: "https://prism.example"}, WithoutCloudTelemetry(), WithEnforcer(usageChecker{}))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ctx, usage := usageContext(g, "turn", "session")
	base := usageBase(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "prism.example" || !strings.Contains(req.Header.Get("X-Destination"), "/models/model:generateContent") {
			t.Error("proxy destination changed")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":14}}`))}, nil
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/models/model:generateContent", strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := g.WrapRoundTripper(base).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	got := usage.finish()
	if got == nil || got.ModelCalls != 1 || got.ByModel[0].Model != "model" || *got.TotalTokens != 14 {
		t.Fatalf("proxy counted twice or lost original model: %+v", got)
	}
}

func TestInvocationUsageGeminiReasoningIsIncludedInOutput(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		for _, thoughts := range []string{"6", "0", "null", "-1", "9007199254740992"} {
			raw := `{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"thoughtsTokenCount":` + thoughts + `,"totalTokenCount":20}}`
			if streamed {
				raw = "data: " + raw + "\n\n"
			}
			counts, partial := observedModelUsage([]byte(raw), streamed)
			if thoughts == "-1" || thoughts == "9007199254740992" {
				if counts != nil {
					t.Fatalf("invalid thoughts accepted: %s", thoughts)
				}
				continue
			}
			if counts == nil || partial {
				t.Fatalf("measured Gemini usage missing: %+v", counts)
			}
			if thoughts == "null" {
				if counts.ReasoningTokens != nil || *counts.OutputTokens != 4 {
					t.Fatal("unknown reasoning became measured")
				}
				continue
			}
			want := uint64(6)
			if thoughts == "0" {
				want = 0
			}
			if counts.ReasoningTokens == nil || *counts.ReasoningTokens != want || *counts.OutputTokens != 4+want || *counts.TotalTokens != 20 {
				t.Fatalf("reasoning omitted from output: %+v", counts)
			}
		}
	}
	counts, _ := observedModelUsage([]byte(`{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":14}}`), false)
	if counts == nil || counts.ReasoningTokens != nil {
		t.Fatal("absent reasoning became zero")
	}
}
