// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	flyedge "github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
)

// respBase returns a canned provider response (streaming or not) so we can exercise the post_llm
// path without a network.
type respBase struct {
	stream bool
	body   string
}

func (b *respBase) RoundTrip(*http.Request) (*http.Response, error) {
	h := make(http.Header)
	if b.stream {
		h.Set("Content-Type", "text/event-stream")
	} else {
		h.Set("Content-Type", "application/json")
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(b.body))}, nil
}

// stageEnforcer allows pre_llm but denies post_llm (so we isolate response-side blocking).
type stageEnforcer struct{ last string }

func (s *stageEnforcer) Check(_ context.Context, req enforce.CheckRequest) (enforce.Decision, error) {
	s.last = string(req.Stage)
	if req.Stage == flyedge.StagePostLLM {
		return enforce.Decision{Action: flyedge.ActionDeny, Reason: "output_policy"}, nil
	}
	return enforce.Decision{Action: flyedge.ActionAllow}, nil
}

func TestPostLLMBlocksNonStreaming(t *testing.T) {
	enf := &stageEnforcer{}
	g, _ := flyedge.New(flyedge.Config{}, flyedge.WithEnforcer(enf))
	base := &respBase{body: `{"choices":[{"message":{"content":"leaked secret sk-123"}}]}`}
	client := &http.Client{Transport: g.WrapRoundTripper(base, flyedge.WithResponseCheck())}

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	_, err := client.Do(req)
	if de, ok := flyedge.AsDenyError(err); !ok || de.Decision.Reason != "output_policy" {
		t.Fatalf("post_llm deny should block the response; got err=%v", err)
	}
	if enf.last != "post_llm" {
		t.Errorf("last stage = %q, want post_llm", enf.last)
	}
}

func TestPostLLMBlocksNonStreamingGemini(t *testing.T) {
	enf := &stageEnforcer{}
	g, _ := flyedge.New(flyedge.Config{}, flyedge.WithEnforcer(enf))
	base := &respBase{body: `{"candidates":[{"content":{"parts":[{"text":"leaked secret sk-123"}],"role":"model"}}]}`}
	client := &http.Client{Transport: g.WrapRoundTripper(base, flyedge.WithResponseCheck())}

	req, _ := http.NewRequest(http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-pro:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	_, err := client.Do(req)
	if de, ok := flyedge.AsDenyError(err); !ok || de.Decision.Reason != "output_policy" {
		t.Fatalf("post_llm deny should block the response; got err=%v", err)
	}
	if enf.last != "post_llm" {
		t.Errorf("last stage = %q, want post_llm", enf.last)
	}
}

func TestPostLLMMonitorsStreamingGemini(t *testing.T) {
	enf := &stageEnforcer{}
	g, _ := flyedge.New(flyedge.Config{}, flyedge.WithEnforcer(enf))
	sse := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello \"}],\"role\":\"model\"}}]}\n\n" +
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"world\"}],\"role\":\"model\"}}]}\n\n"
	base := &respBase{stream: true, body: sse}
	client := &http.Client{Transport: g.WrapRoundTripper(base, flyedge.WithResponseCheck())}

	req, _ := http.NewRequest(http.MethodPost, "https://generativelanguage.googleapis.com/v1beta/models/gemini-1.5-pro:streamGenerateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("streaming should NOT error (monitor-only): %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(got), "hello") {
		t.Errorf("stream body not passed through: %q", got)
	}
	_ = resp.Body.Close()
	if enf.last != "post_llm" {
		t.Errorf("post_llm check should run on stream close; last=%q", enf.last)
	}
}

func TestPostLLMMonitorsStreaming(t *testing.T) {
	// Streaming: the response is delivered (monitor-only), and the post_llm check runs on Close
	// over the accumulated completion — it cannot block already-sent tokens.
	enf := &stageEnforcer{}
	g, _ := flyedge.New(flyedge.Config{}, flyedge.WithEnforcer(enf))
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\ndata: [DONE]\n\n"
	base := &respBase{stream: true, body: sse}
	client := &http.Client{Transport: g.WrapRoundTripper(base, flyedge.WithResponseCheck())}

	req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("streaming should NOT error (monitor-only): %v", err)
	}
	got, _ := io.ReadAll(resp.Body)      // caller receives the full stream unchanged
	if !strings.Contains(string(got), "hello") {
		t.Errorf("stream body not passed through: %q", got)
	}
	_ = resp.Body.Close()                // Close triggers the post_llm check over "hello world"
	if enf.last != "post_llm" {
		t.Errorf("post_llm check should run on stream close; last=%q", enf.last)
	}
}
