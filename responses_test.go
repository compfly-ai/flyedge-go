// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

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

type responsesEnforcer struct {
	requests   []CheckRequest
	identities []map[string]string
	deny       Stage
	action     Action
	err        error
}

func (e *responsesEnforcer) Check(ctx context.Context, req enforce.CheckRequest) (enforce.Decision, error) {
	e.requests = append(e.requests, req)
	e.identities = append(e.identities, enforce.IdentityHeaders(ctx))
	action := e.action
	if action == "" {
		action = ActionAllow
	}
	if req.Stage == e.deny {
		action = ActionDeny
	}
	return Decision{Action: action, Reason: "fixture"}, e.err
}

type responsesBase struct {
	calls      int
	reply      string
	stream     bool
	status     int
	body       string
	closeCount int
}
type responsesReadCloser struct {
	io.Reader
	close func()
}

func (r responsesReadCloser) Close() error { r.close(); return nil }
func (b *responsesBase) RoundTrip(req *http.Request) (*http.Response, error) {
	b.calls++
	raw, _ := io.ReadAll(req.Body)
	b.body = string(raw)
	h := make(http.Header)
	if b.stream {
		h.Set("Content-Type", "text/event-stream")
	}
	status := b.status
	if status == 0 {
		status = 200
	}
	return &http.Response{StatusCode: status, Header: h, Body: responsesReadCloser{strings.NewReader(b.reply), func() { b.closeCount++ }}}, nil
}

const responsesRequest = `{
 "model":"fixture", "instructions":"trusted instructions", "input":[
 {"role":"user","content":[{"type":"input_text","text":"question"}]},
 {"type":"function_call_output","call_id":"c1","output":"untrusted result"}]}`
const responsesReply = `{"status":"completed","output":[{"type":"function_call","name":"send_email","call_id":"c2","arguments":"{\"body\":\"secret\"}"}]}`

func responseReq(t *testing.T, body string) *http.Request {
	t.Helper()
	ctx := ContextWithSession(context.Background(), "responses-session")
	ctx = ContextWithPrincipal(ctx, Principal{Provider: "test", UPN: "user@example.test"})
	ctx = ContextWithDelegation(ctx, "test-delegation")
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}
func responsesGuard(t *testing.T, cfg Config, e *responsesEnforcer) *Guard {
	t.Helper()
	g, err := New(cfg, WithEnforcer(e))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}
func TestResponsesDenyAllowAndContext(t *testing.T) {
	for _, deny := range []Stage{StagePreLLM, StagePostLLM, ""} {
		t.Run(string(deny), func(t *testing.T) {
			e := &responsesEnforcer{deny: deny}
			g := responsesGuard(t, Config{FailMode: FailClosed}, e)
			base := &responsesBase{reply: responsesReply}
			req := responseReq(t, responsesRequest)
			resp, err := g.WrapRoundTripper(base).RoundTrip(req)
			if deny != "" {
				if _, ok := AsDenyError(err); !ok {
					t.Fatalf("want denial, got %v", err)
				}
				if resp != nil {
					t.Fatal("denied output released")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				got, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if string(got) != responsesReply {
					t.Fatal("output changed")
				}
			}
			wantCalls, wantChecks := 1, 2
			if deny == StagePreLLM {
				wantCalls, wantChecks = 0, 1
			}
			if g.Report().Checks != wantChecks {
				t.Fatal("policy telemetry missing")
			}
			if base.calls != wantCalls || len(e.requests) != wantChecks {
				t.Fatalf("calls=%d checks=%d", base.calls, len(e.requests))
			}
			if wantCalls > 0 && (base.body != responsesRequest || base.closeCount != 1) {
				t.Fatal("body changed or response not closed")
			}
			replay, _ := req.GetBody()
			replayBody, _ := io.ReadAll(replay)
			_ = replay.Close()
			if string(replayBody) != responsesRequest {
				t.Fatal("request replay changed")
			}
			for i, r := range e.requests {
				if r.SessionID != "responses-session" || r.Operation.Type != "responses" || r.Operation.ModelID != "fixture" || r.Provider != "openai" {
					t.Fatalf("bad attribution: %+v", r)
				}
				if e.identities[i][enforce.HeaderDelegationToken] != "test-delegation" || e.identities[i][enforce.HeaderOBOPrincipal] == "" {
					t.Fatal("identity lost")
				}
			}
			for _, part := range []string{"trusted instructions", "question", "untrusted result", "c1"} {
				if !strings.Contains(e.requests[0].Content.Full, part) {
					t.Fatalf("missing %s", part)
				}
			}
			if wantChecks == 2 && !strings.Contains(e.requests[1].Content.Full, "send_email") {
				t.Fatal("tool proposal missing")
			}
		})
	}
}
func TestResponsesWarnEmptyOutputAndFailModes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   FailMode
		action Action
		err    error
		deny   bool
	}{
		{"warn", FailClosed, ActionWarn, nil, false}, {"closed", FailClosed, "", errors.New("unavailable"), true}, {"open", FailOpen, "", errors.New("unavailable"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &responsesEnforcer{action: tc.action, err: tc.err}
			g := responsesGuard(t, Config{FailMode: tc.mode}, e)
			base := &responsesBase{reply: `{"status":"completed","output":[]}`}
			resp, err := g.WrapRoundTripper(base).RoundTrip(responseReq(t, `{"model":"fixture","input":"hello"}`))
			if tc.deny {
				if err == nil || base.calls != 0 {
					t.Fatal("fail-closed bypass")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if len(e.requests) != 2 {
					t.Fatal("empty output not audited")
				}
			}
		})
	}
}
func TestResponsesMultiRoundAndPartialContext(t *testing.T) {
	e := &responsesEnforcer{}
	g := responsesGuard(t, Config{}, e)
	base := &responsesBase{reply: responsesReply}
	rt := g.WrapRoundTripper(base)
	for _, body := range []string{`{"model":"fixture","input":"question"}`, `{"model":"fixture","previous_response_id":"resp_prior","input":[{"type":"function_call_output","call_id":"c2","output":"result"}]}`, `{"conversation":"conv1","input":[]}`, `{"input":[{"type":"item_reference","id":"item1"}]}`} {
		resp, err := rt.RoundTrip(responseReq(t, body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	for i, r := range e.requests {
		if r.Metadata["context_partial"] != (i >= 2) {
			t.Fatal("wrong context visibility")
		}
	}
	if !strings.Contains(e.requests[2].Content.Full, "result") {
		t.Fatal("function result missing")
	}
}
func TestResponsesUnsupportedModesAndProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, reply string
		proxy             bool
		mode              ModelMode
		status            int
		pre               bool
	}{
		{name: "proxy", body: responsesRequest, proxy: true}, {name: "remote gateway", body: responsesRequest, mode: ModelModeGateway},
		{name: "background", body: `{"background":true}`}, {name: "malformed request", body: `[]`},
		{name: "failed", body: responsesRequest, reply: `{"status":"failed","error":{},"output":[]}`, pre: true},
		{name: "queued", body: responsesRequest, reply: `{"status":"queued","output":[]}`, pre: true},
		{name: "malformed reply", body: responsesRequest, reply: `not json`, pre: true},
		{name: "missing output", body: responsesRequest, reply: `{}`, pre: true},
		{name: "http error", body: responsesRequest, reply: `{"error":"limited"}`, status: 429, pre: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &responsesEnforcer{}
			g := responsesGuard(t, Config{ProxyMode: tc.proxy}, e)
			g.modelMode = tc.mode
			base := &responsesBase{reply: tc.reply, status: tc.status}
			resp, err := g.WrapRoundTripper(base).RoundTrip(responseReq(t, tc.body))
			if tc.status == 429 {
				if err != nil || resp.StatusCode != 429 {
					t.Fatal("provider error changed")
				}
				_ = resp.Body.Close()
			} else if err == nil {
				t.Fatal("unsupported/invalid call succeeded")
			}
			want := 0
			if tc.pre {
				want = 1
			}
			if len(e.requests) != want || base.calls != want {
				t.Fatalf("checks=%d calls=%d", len(e.requests), base.calls)
			}
		})
	}
}
func TestResponsesStreamingAudit(t *testing.T) {
	prefix := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"secret\"}\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"payload\"}\n\n"
	terminal := "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"function_call\",\"name\":\"send_email\",\"arguments\":\"secret payload\"}]}}\n\n"
	for _, mode := range []string{"complete", "partial", "truncated", "failed"} {
		t.Run(mode, func(t *testing.T) {
			e := &responsesEnforcer{deny: StagePostLLM}
			g := responsesGuard(t, Config{}, e)
			raw := prefix + terminal
			if mode == "truncated" {
				raw = prefix + strings.Repeat(": keepalive\n", responsesCaptureLimit/10) + terminal
			}
			if mode == "failed" {
				raw = prefix + `data: {"type":"response.failed","response":{"output":[]}}` + "\n\n"
			}
			base := &responsesBase{reply: raw, stream: true}
			req := responseReq(t, responsesRequest)
			ctx, cancel := context.WithCancel(req.Context())
			defer cancel()
			req = req.WithContext(ctx)
			resp, err := g.WrapRoundTripper(base).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "partial" {
				buf := make([]byte, len(prefix))
				_, err = io.ReadFull(resp.Body, buf)
				if err != nil || string(buf) != prefix {
					t.Fatal("stream changed")
				}
				cancel()
			} else {
				got, err := io.ReadAll(resp.Body)
				if err != nil || string(got) != raw {
					t.Fatal("stream changed")
				}
			}
			_ = resp.Body.Close()
			_ = resp.Body.Close()
			if len(e.requests) != 2 || base.closeCount != 1 {
				t.Fatalf("checks=%d closes=%d", len(e.requests), base.closeCount)
			}
			post := e.requests[1]
			if !strings.Contains(post.Content.Full, "secret") || !strings.Contains(post.Content.Full, "payload") {
				t.Fatal("missing stream content")
			}
			if post.Metadata["capture_incomplete"] != (mode != "complete") || post.Metadata["capture_truncated"] != (mode == "truncated") || post.Metadata["verdict_relay"] != "stream_delivered" {
				t.Fatalf("bad stream flags: %+v", post.Metadata)
			}
			if e.identities[1][enforce.HeaderDelegationToken] != "test-delegation" {
				t.Fatal("cancel erased identity")
			}
		})
	}
}
func TestResponsesInjectionAndEncryptedReasoning(t *testing.T) {
	body := map[string]any{"instructions": "original", "input": "hello"}
	injectResponsesInstructions(body, "injected")
	if body["instructions"] != "injected\n\noriginal" || body["input"] != "hello" {
		t.Fatal("injection lost input")
	}
	text, incomplete, err := responsesCompletion([]byte(`{"status":"incomplete","output":[{"type":"reasoning","encrypted_content":"opaque"},{"type":"message","content":[{"type":"output_text","text":"answer"}]}]}`))
	if err != nil || !incomplete || strings.Contains(text, "opaque") || !strings.Contains(text, "answer") {
		t.Fatalf("%q %v %v", text, incomplete, err)
	}
}
func TestResponsesCompatibleRoutesAndNonGenerationPassthrough(t *testing.T) {
	for _, url := range []string{"https://gateway.example/responses", "https://azure.example/openai/v1/responses"} {
		e := &responsesEnforcer{deny: StagePreLLM}
		g := responsesGuard(t, Config{}, e)
		base := &responsesBase{}
		req := responseReq(t, responsesRequest)
		parsed, _ := http.NewRequest("POST", url, nil)
		req.URL = parsed.URL
		_, err := g.WrapRoundTripper(base).RoundTrip(req)
		if err == nil || base.calls != 0 {
			t.Fatal("compatible endpoint bypass")
		}
	}
	for _, path := range []string{"/v1/responses/id", "/v1/responses/compact"} {
		e := &responsesEnforcer{deny: StagePreLLM}
		g := responsesGuard(t, Config{}, e)
		base := &responsesBase{reply: `{}`}
		req := responseReq(t, `{}`)
		req.URL.Path = path
		resp, err := g.WrapRoundTripper(base).RoundTrip(req)
		if err != nil || len(e.requests) != 0 {
			t.Fatal("non-generation endpoint classified")
		}
		_ = resp.Body.Close()
	}
}
