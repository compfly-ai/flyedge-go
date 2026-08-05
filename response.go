// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// Response-side (post_llm) inspection for the transport wrap. Opt in with WithResponseCheck.
//
// Non-streaming responses can be BLOCKED: the wrap buffers the completion, runs a post_llm check,
// and returns a *DenyError instead of the response on a deny (the model already generated it, but
// the caller never sees it). Streaming responses (SSE) are MONITORED, not blocked: tokens already
// sent to the caller can't be retracted, so the wrap accumulates the streamed text and runs one
// post_llm check when the stream ends (for telemetry/audit + alerting). This asymmetry is inherent
// to streaming, and is surfaced honestly rather than pretending to block.

// WrapOption customizes WrapRoundTripper.
type WrapOption func(*guardRoundTripper)

// WithResponseCheck enables a post_llm policy check on model responses (block for non-streaming,
// monitor for streaming). Off by default — request-side (pre_llm) checking is always on.
func WithResponseCheck() WrapOption {
	return func(rt *guardRoundTripper) { rt.checkResponse = true }
}

// respExtractor pulls the completion text from a non-streaming response body for a provider.
type respExtractor func(body []byte) string

func respExtractorFor(host string) respExtractor {
	switch {
	case strings.Contains(host, "anthropic.com"):
		return completionAnthropic
	case strings.Contains(host, "openai.com"):
		return completionOpenAI
	case strings.Contains(host, "generativelanguage.googleapis.com"):
		return completionGemini
	default:
		return completionOpenAI // OpenAI-compatible default
	}
}

func completionAnthropic(body []byte) string {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &r) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Content {
		if c.Text != "" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

func completionGemini(body []byte) string {
	var r struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(body, &r) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Candidates {
		for _, p := range c.Content.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func completionOpenAI(body []byte) string {
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &r) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range r.Choices {
		b.WriteString(c.Message.Content)
	}
	return b.String()
}

// streamMonitor wraps a streaming (SSE) response body: it passes bytes through to the caller
// unchanged while teeing them into a buffer, then on Close extracts the accumulated completion text
// and hands it to done() (which runs the post_llm check). It never blocks or alters the stream —
// sent tokens can't be un-sent — so this is monitor + record, not enforce.
type streamMonitor struct {
	rc   io.ReadCloser
	buf  bytes.Buffer
	host string
	done func(completion string)
}

func newStreamMonitor(rc io.ReadCloser, host string, done func(string)) *streamMonitor {
	return &streamMonitor{rc: rc, host: host, done: done}
}

func (m *streamMonitor) Read(p []byte) (int, error) {
	n, err := m.rc.Read(p)
	if n > 0 {
		m.buf.Write(p[:n])
	}
	return n, err
}

func (m *streamMonitor) Close() error {
	err := m.rc.Close()
	if m.done != nil {
		m.done(sseCompletion(m.host, m.buf.Bytes()))
	}
	return err
}

// sseCompletion reconstructs the completion text from accumulated SSE bytes, for any of the three
// providers' delta formats (Anthropic content_block_delta{delta.text}; OpenAI
// choices[].delta.content; Gemini streamGenerateContent?alt=sse repeats the full
// candidates[].content.parts[].text shape per chunk rather than a delta). host picks the Gemini
// parse path since its per-chunk shape overlaps structurally with the others' field names.
func sseCompletion(host string, raw []byte) string {
	gemini := strings.Contains(host, "generativelanguage.googleapis.com")
	var b strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		if gemini {
			b.WriteString(completionGemini([]byte(data)))
			continue
		}
		// Anthropic: {"type":"content_block_delta","delta":{"type":"text_delta","text":"..."}}
		// OpenAI:    {"choices":[{"delta":{"content":"..."}}]}
		var ev struct {
			Delta struct {
				Text    string `json:"text"`
				Content string `json:"content"`
			} `json:"delta"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		b.WriteString(ev.Delta.Text)
		b.WriteString(ev.Delta.Content)
		for _, c := range ev.Choices {
			b.WriteString(c.Delta.Content)
		}
	}
	return b.String()
}
