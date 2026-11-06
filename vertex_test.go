// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
)

type vertexCase struct {
	URL, Provider, Model string
	Request, Response    json.RawMessage
}

func vertexCases(t *testing.T) []vertexCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/vertex.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []vertexCase
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

type vertexEnforcer struct {
	requests []enforce.CheckRequest
	deny     string
}

func (e *vertexEnforcer) Check(_ context.Context, r enforce.CheckRequest) (enforce.Decision, error) {
	e.requests = append(e.requests, r)
	action := enforce.ActionAllow
	if string(r.Stage) == e.deny {
		action = enforce.ActionDeny
	}
	return enforce.Decision{Action: action}, nil
}

type vertexBase func(*http.Request) (*http.Response, error)

func (f vertexBase) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type vertexSigner struct{ body []byte }

func (s *vertexSigner) DID() string   { return "did:test:vertex" }
func (s *vertexSigner) KeyID() string { return "fixture" }
func (s *vertexSigner) Sign(body []byte, _ time.Time) (map[string]string, error) {
	s.body = append([]byte(nil), body...)
	return map[string]string{"X-Fixture-Signature": "signed"}, nil
}

func TestVertexTransport(t *testing.T) {
	for _, tc := range vertexCases(t) {
		for _, deny := range []string{"", "pre_llm", "post_llm"} {
			t.Run(tc.Provider+tc.URL+deny, func(t *testing.T) {
				rec := &vertexEnforcer{deny: deny}
				cloud := false
				g, err := flyedge.New(flyedge.Config{Mode: flyedge.ModeEnforce, FailMode: flyedge.FailClosed, CloudTelemetry: &cloud}, flyedge.WithEnforcer(rec))
				if err != nil {
					t.Fatal(err)
				}
				defer g.Close()
				calls := 0
				base := vertexBase(func(r *http.Request) (*http.Response, error) {
					calls++
					body, _ := io.ReadAll(r.Body)
					if !bytes.Equal(body, tc.Request) {
						t.Error("request changed")
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(tc.Response))}, nil
				})
				req, _ := http.NewRequest("POST", tc.URL, bytes.NewReader(tc.Request))
				resp, err := g.WrapRoundTripper(base).RoundTrip(req)
				if deny != "" {
					var blocked *flyedge.DenyError
					if !errors.As(err, &blocked) {
						t.Fatalf("expected deny, got %v", err)
					}
					if resp != nil {
						t.Fatal("denied response delivered")
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					got, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if !bytes.Equal(got, tc.Response) {
						t.Error("response changed")
					}
				}
				want := 1
				if deny == "pre_llm" {
					want = 0
				}
				if calls != want {
					t.Fatalf("provider calls=%d want %d", calls, want)
				}
				if len(rec.requests) != want+1 {
					t.Fatalf("checks=%d", len(rec.requests))
				}
				for i, r := range rec.requests {
					if r.Operation.ModelID != tc.Model {
						t.Errorf("model=%q", r.Operation.ModelID)
					}
					text := "hello"
					if i == 1 {
						text = "answer"
					}
					if r.Content.Full != text {
						t.Errorf("content=%q want %q", r.Content.Full, text)
					}
				}
			})
		}
	}
}
func TestVertexGateway(t *testing.T) {
	for _, tc := range vertexCases(t) {
		t.Run(tc.URL, func(t *testing.T) {
			signer := &vertexSigner{}
			cloud := false
			g, err := flyedge.New(flyedge.Config{APIURL: "https://gateway.example", ProxyMode: true, CloudTelemetry: &cloud}, flyedge.WithSigner(signer))
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			base := vertexBase(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != "https://gateway.example/v1/proxy" || r.Header.Get("X-Destination") != tc.URL {
					t.Fatalf("incorrect gateway route: %s", r.URL)
				}
				if r.Header.Get("X-Fixture-Signature") != "signed" || !bytes.Equal(signer.body, tc.Request) {
					t.Error("body not signed")
				}
				if r.Header.Get("X-Session-ID") != "vertex-session" {
					t.Error("session lost")
				}
				body, _ := io.ReadAll(r.Body)
				if !bytes.Equal(body, tc.Request) {
					t.Error("body changed")
				}
				return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("denied"))}, nil
			})
			req, _ := http.NewRequest("POST", tc.URL, bytes.NewReader(tc.Request))
			req = req.WithContext(flyedge.ContextWithSession(req.Context(), "vertex-session"))
			resp, err := g.WrapRoundTripper(base).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 403 {
				t.Fatal("gateway error replaced")
			}
		})
	}
}

func TestVertexStreamAudit(t *testing.T) {
	for _, tc := range vertexCases(t) {
		if !strings.Contains(tc.URL, "stream") {
			continue
		}
		t.Run(tc.Provider, func(t *testing.T) {
			rec := &vertexEnforcer{deny: "post_llm"}
			cloud := false
			g, err := flyedge.New(flyedge.Config{Mode: flyedge.ModeEnforce, CloudTelemetry: &cloud}, flyedge.WithEnforcer(rec))
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			chunk := string(tc.Response)
			if tc.Provider == "anthropic" {
				chunk = `{"type":"content_block_delta","delta":{"type":"text_delta","text":"answer"}}`
			}
			wire := "data: " + chunk + "\n\ndata: [DONE]\n\n"
			base := vertexBase(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
			})
			req, _ := http.NewRequest("POST", tc.URL, bytes.NewReader(tc.Request))
			resp, err := g.WrapRoundTripper(base).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err = resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if string(got) != wire {
				t.Fatal("stream bytes changed")
			}
			if len(rec.requests) != 2 {
				t.Fatalf("checks=%d", len(rec.requests))
			}
			if rec.requests[1].Content.Full != "answer" || rec.requests[1].Operation.ModelID != tc.Model {
				t.Fatalf("wrong stream audit: %+v", rec.requests[1])
			}
		})
	}
}
func TestVertexUnsupportedRoutesPassThrough(t *testing.T) {
	for _, url := range []string{
		"https://aiplatform.googleapis.com.evil.example/v1/publishers/google/models/m:generateContent",
		"https://evil-aiplatform.googleapis.com.evil.example/v1/publishers/google/models/m:generateContent",
		"https://aiplatform.googleapis.com/v1/publishers/google/models/m:countTokens",
		"https://aiplatform.googleapis.com/v1/publishers/google/models/m:predict",
		"https://aiplatform.googleapis.com/v1/publishers/other/models/m:rawPredict",
		"https://aiplatform.googleapis.com/v2/publishers/google/models/m:generateContent",
		"https://aiplatform.googleapis.com/v1/publishers/google/models/:generateContent",
	} {
		t.Run(url, func(t *testing.T) {
			cloud := false
			rec := &vertexEnforcer{deny: "pre_llm"}
			g, err := flyedge.New(flyedge.Config{ProxyMode: true, CloudTelemetry: &cloud}, flyedge.WithEnforcer(rec))
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			base := vertexBase(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != url {
					t.Fatal("unrelated request rerouted")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})
			req, _ := http.NewRequest("POST", url, strings.NewReader("{}"))
			resp, err := g.WrapRoundTripper(base).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if len(rec.requests) != 0 {
				t.Fatal("unrelated request checked")
			}
		})
	}
}
