package flyedge

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type proxyPolicyTransport func(*http.Request) (*http.Response, error)

func (f proxyPolicyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type proxyPolicyBody struct {
	io.Reader
	closed bool
	reads  int
}

func (b *proxyPolicyBody) Read(p []byte) (int, error) { b.reads++; return b.Reader.Read(p) }
func (b *proxyPolicyBody) Close() error               { b.closed = true; return nil }

func TestProxyPolicyDenialAndWarning(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, denied := range []bool{false, true} {
			t.Run(path+"/"+map[bool]string{true: "deny", false: "warn"}[denied], func(t *testing.T) {
				var decisions []Decision
				var checks []CheckRequest
				g, err := New(Config{APIURL: "https://gateway.example", ModelMode: ModelModePassthrough}, WithEnforcer(&responsesEnforcer{}), WithDecisionHandler(func(r CheckRequest, d Decision) { checks = append(checks, r); decisions = append(decisions, d) }))
				if err != nil {
					t.Fatal(err)
				}
				defer g.Close()
				body := &proxyPolicyBody{Reader: strings.NewReader("data: streaming provider bytes\n\n")}
				base := proxyPolicyTransport(func(req *http.Request) (*http.Response, error) {
					if req.URL.String() != "https://gateway.example/v1/proxy" {
						t.Fatal(req.URL)
					}
					h := http.Header{"Content-Type": []string{"text/event-stream"}}
					status := 200
					if denied {
						status = 403
						body.Reader = strings.NewReader(`{"error":{"type":"policy_denied","message":"Use approved tools. ✓","reason":"restricted","warnings":["rule"]}}`)
					} else {
						h.Set("X-CompFly-Policy-Warnings", base64.StdEncoding.EncodeToString([]byte(`[{"stage":"pre_llm","message":"Proceed carefully. ✓","reason":"advisory","warnings":["rule"]}]`)))
					}
					return &http.Response{StatusCode: status, Header: h, Body: body}, nil
				})
				req, _ := http.NewRequest("POST", "https://api.openai.com"+path, strings.NewReader(`{"model":"fixture","messages":[],"input":"hi"}`))
				req.Header.Set("X-Session-ID", "fixture-session")
				resp, err := (&http.Client{Transport: g.WrapRoundTripper(base)}).Do(req)
				if denied {
					de, ok := AsDenyError(err)
					if !ok || de.Decision.Message != "Use approved tools. ✓" || de.Decision.Reason != "restricted" {
						t.Fatalf("want authored denial: %v", err)
					}
					if resp != nil || !body.closed {
						t.Fatal("denied body not closed")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if body.reads != 0 {
						t.Fatal("warning consumed streaming body")
					}
					raw, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if string(raw) != "data: streaming provider bytes\n\n" {
						t.Fatal(string(raw))
					}
				}
				if len(decisions) != 1 || len(checks) != 1 {
					t.Fatalf("decisions=%v checks=%v", decisions, checks)
				}
				if checks[0].Stage != StagePreLLM || checks[0].SessionID != "fixture-session" || checks[0].Operation.ModelID != "fixture" {
					t.Fatalf("context: %+v", checks[0])
				}
				if !denied && (decisions[0].Action != ActionWarn || decisions[0].Message != "Proceed carefully. ✓") {
					t.Fatal(decisions[0])
				}
			})
		}
	}
}

func TestProxyNonPolicyResponsesPreserved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"provider", 403, `{"error":{"type":"authentication_error","message":"bad token"}}`},
		{"wrong status", 500, `{"error":{"type":"policy_denied","message":"no"}}`},
		{"missing message", 403, `{"error":{"type":"policy_denied"}}`},
		{"malformed", 403, `{"error":`},
		{"wrong warnings", 403, `{"error":{"type":"policy_denied","message":"no","warnings":[42]}}`},
		{"large", 403, strings.Repeat("x", 128*1024)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := responsesGuard(t, Config{APIURL: "https://gateway.example", ModelMode: ModelModePassthrough}, &responsesEnforcer{})
			body := &proxyPolicyBody{Reader: strings.NewReader(tc.body)}
			base := proxyPolicyTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: body}, nil
			})
			resp, err := g.WrapRoundTripper(base).RoundTrip(responseReq(t, responsesRequest))
			if err != nil {
				t.Fatal(err)
			}
			if body.closed {
				t.Fatal("body prematurely closed")
			}
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || !bytes.Equal(raw, []byte(tc.body)) || !body.closed || resp.StatusCode != tc.status {
				t.Fatal("provider response changed")
			}
		})
	}
}

func TestProxyWarningMetadataValidation(t *testing.T) {
	for _, raw := range []string{"bad", strings.Repeat("x", 17*1024), base64.StdEncoding.EncodeToString([]byte(`[{"stage":"tool_call","message":"no","reason":"r","warnings":["w"]}]`)), base64.StdEncoding.EncodeToString([]byte(`[{"stage":"pre_llm","warnings":["w"]}]`))} {
		g, err := New(Config{APIURL: "https://gateway.example", ModelMode: ModelModePassthrough}, WithEnforcer(&responsesEnforcer{}), WithDecisionHandler(func(CheckRequest, Decision) { t.Error("malformed warning delivered") }))
		if err != nil {
			t.Fatal(err)
		}
		base := proxyPolicyTransport(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"X-Compfly-Policy-Warnings": []string{raw}}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})
		resp, err := g.WrapRoundTripper(base).RoundTrip(responseReq(t, responsesRequest))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		g.Close()
	}
}

func TestDirectProviderCannotAssertProxyPolicy(t *testing.T) {
	g := responsesGuard(t, Config{ModelMode: ModelModeCheck}, &responsesEnforcer{})
	base := proxyPolicyTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"type":"policy_denied","message":"spoof"}}`))}, nil
	})
	resp, err := g.WrapRoundTripper(base).RoundTrip(responseReq(t, responsesRequest))
	var denied *DenyError
	if errors.As(err, &denied) || resp == nil {
		t.Fatalf("provider denial reclassified: %v", err)
	}
	resp.Body.Close()
}

func TestProxyEmptyMessageAndReadFailure(t *testing.T) {
	g := responsesGuard(t, Config{APIURL: "https://gateway.example", ModelMode: ModelModePassthrough}, &responsesEnforcer{})
	transport := g.WrapRoundTripper(proxyPolicyTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"type":"policy_denied","message":""}}`))}, nil
	}))
	_, err := transport.RoundTrip(responseReq(t, responsesRequest))
	de, ok := AsDenyError(err)
	if !ok || de.Decision.Message != "" {
		t.Fatalf("empty authored denial: %v", err)
	}
	if _, ok := invocationPolicyDenial(err); !ok {
		t.Fatal("denial not recognized by reverse invocation")
	}
	readErr := errors.New("broken body")
	body := &proxyPolicyBody{Reader: proxyErrorReader{readErr}}
	transport = g.WrapRoundTripper(proxyPolicyTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Header: make(http.Header), Body: body}, nil
	}))
	resp, err := transport.RoundTrip(responseReq(t, responsesRequest))
	if !errors.Is(err, readErr) || resp != nil || !body.closed {
		t.Fatalf("read error not preserved: %v", err)
	}
}

type proxyErrorReader struct{ err error }

func (r proxyErrorReader) Read([]byte) (int, error) { return 0, r.err }
