// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// Responses has its own protocol: typed input/output items and named SSE events.
// Keep it separate from Chat Completions so endpoint recognition cannot select
// the old choices/messages extractors by accident.
func (t *guardRoundTripper) roundTripResponses(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		return nil, fmt.Errorf("Responses request body is required")
	}
	raw, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return nil, fmt.Errorf("Responses request must be a JSON object")
	}
	if body["background"] == true {
		return nil, fmt.Errorf("Responses background mode is unsupported: output retrieval is not governed")
	}
	if t.guard.cfg.ProxyMode || t.guard.ModelMode() != ModelModeCheck {
		return nil, fmt.Errorf("Responses proxy mode is unsupported until the gateway implements Responses governance")
	}
	partial := body["previous_response_id"] != nil || body["conversation"] != nil
	if items, ok := body["input"].([]any); ok {
		for _, item := range items {
			if m, ok := item.(map[string]any); ok && m["type"] == "item_reference" {
				partial = true
			}
		}
	}
	if t.guard.simCtl != nil {
		if injected, ok := t.guard.simCtl.InjectLLMSystemMessage(req.URL.Host); ok {
			injectResponsesInstructions(body, injected)
			raw, err = json.Marshal(body)
			if err != nil {
				return nil, err
			}
		}
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	requestBody := raw
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(requestBody)), nil }
	model, _ := body["model"].(string)
	session := t.resolveSession(req)
	check := func(ctx context.Context, stage Stage, text string, metadata map[string]any) error {
		_, err := t.guard.Check(ctx, CheckRequest{
			RequestID: "req-" + randHex(), SessionID: session, Stage: stage,
			ComponentType: "LLM", ComponentName: req.URL.Host, MethodName: "http",
			Provider: "openai", Content: Content{Full: text},
			Operation: Operation{Type: "responses", ModelID: model}, Metadata: metadata,
		})
		return err
	}
	if err := check(req.Context(), StagePreLLM, responsesPrompt(body), map[string]any{"context_partial": partial}); err != nil {
		return nil, err
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || !t.checkResponse || resp.StatusCode >= 400 {
		return resp, err
	}
	if resp.Body == nil {
		return nil, fmt.Errorf("Responses output body is missing")
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("compressed Responses SSE is unsupported")
		}
		// Cancellation must not erase audit of bytes already delivered. Guard.Check
		// supplies the policy timeout; context values retain session/OBO/trace identity.
		ctx := WithVerdictRelay(context.WithoutCancel(req.Context()), "stream_delivered")
		resp.Body = &responsesMonitor{inner: resp.Body, done: func(raw []byte, truncated, interrupted bool) {
			text, incomplete := responsesSSE(raw)
			_ = check(ctx, StagePostLLM, text, map[string]any{"context_partial": partial,
				"verdict_relay": "stream_delivered", "capture_truncated": truncated, "capture_incomplete": interrupted || incomplete})
		}}
		return resp, nil
	}
	raw, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	text, incomplete, err := responsesCompletion(raw)
	if err != nil {
		return nil, err
	}
	if err := check(req.Context(), StagePostLLM, text, map[string]any{"context_partial": partial,
		"verdict_relay": "relayed", "capture_incomplete": incomplete, "capture_truncated": false}); err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, nil
}

func injectResponsesInstructions(body map[string]any, injected string) {
	original, _ := body["instructions"].(string)
	if original != "" {
		injected += "\n\n" + original
	}
	body["instructions"] = injected
}

// Typed items retain call IDs and function arguments. Encrypted reasoning is
// opaque, and must not be advertised as inspected. References are not fetched.
func visibleResponsesItem(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = visibleResponsesItem(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			if v["type"] == "reasoning" && k == "encrypted_content" {
				continue
			}
			out[k] = visibleResponsesItem(item)
		}
		return out
	default:
		return value
	}
}
func responsesPrompt(body map[string]any) string {
	var parts []string
	if instructions, ok := body["instructions"].(string); ok {
		parts = append(parts, instructions)
	}
	if input := body["input"]; input != nil {
		parts = append(parts, jsonString(visibleResponsesItem(input)))
	}
	return strings.Join(parts, "\n")
}
func responsesOutput(body map[string]any) (string, bool) {
	items, ok := body["output"].([]any)
	if !ok {
		return "", false
	}
	var parts []string
	for _, item := range items {
		if m, ok := item.(map[string]any); ok && m["type"] == "reasoning" {
			if summary, _ := m["summary"].([]any); len(summary) == 0 {
				continue
			}
		}
		parts = append(parts, jsonString(visibleResponsesItem(item)))
	}
	return strings.Join(parts, "\n"), true
}
func responsesCompletion(raw []byte) (string, bool, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", false, fmt.Errorf("invalid Responses output: %w", err)
	}
	switch body["status"] {
	case "failed":
		return "", false, fmt.Errorf("Responses provider reported failure")
	case "queued", "in_progress":
		return "", false, fmt.Errorf("Responses output is not complete; background retrieval is unsupported")
	}
	if body["error"] != nil {
		return "", false, fmt.Errorf("Responses provider reported failure")
	}
	text, ok := responsesOutput(body)
	if !ok {
		return "", false, fmt.Errorf("Responses output array is missing")
	}
	return text, body["status"] == "incomplete", nil
}
func responsesSSE(raw []byte) (string, bool) {
	var prefix strings.Builder
	terminal := ""
	incomplete := true
	for _, line := range strings.Split(string(raw), "\n") {
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(data)), &event) != nil {
			continue
		}
		switch event["type"] {
		case "response.completed", "response.incomplete", "response.failed":
			response, _ := event["response"].(map[string]any)
			var valid bool
			terminal, valid = responsesOutput(response)
			incomplete = event["type"] != "response.completed" || !valid
		case "response.output_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			if delta, ok := event["delta"].(string); ok {
				prefix.WriteString(delta)
			}
		case "response.output_item.added", "response.output_item.done":
			if item, ok := event["item"].(map[string]any); ok && item["type"] != "reasoning" {
				prefix.WriteString(jsonString(visibleResponsesItem(item)))
			}
		}
	}
	if terminal != "" {
		return terminal, incomplete
	}
	return prefix.String(), incomplete
}

const responsesCaptureLimit = 1 << 20

// Capture is bounded; Read passes bytes through untouched. Audit runs exactly
// once, at EOF/error or Close, including early cancellation and truncated capture.
type responsesMonitor struct {
	inner     io.ReadCloser
	mu        sync.Mutex
	capture   bytes.Buffer
	truncated bool
	once      sync.Once
	closeOnce sync.Once
	closeErr  error
	done      func([]byte, bool, bool)
}

func (m *responsesMonitor) Read(p []byte) (int, error) {
	n, err := m.inner.Read(p)
	m.mu.Lock()
	keep := min(n, responsesCaptureLimit-m.capture.Len())
	m.capture.Write(p[:keep])
	m.truncated = m.truncated || keep < n
	m.mu.Unlock()
	if err != nil {
		m.finish(err != io.EOF)
	}
	return n, err
}
func (m *responsesMonitor) finish(interrupted bool) {
	m.once.Do(func() {
		m.mu.Lock()
		raw := bytes.Clone(m.capture.Bytes())
		truncated := m.truncated
		m.mu.Unlock()
		m.done(raw, truncated, interrupted)
	})
}
func (m *responsesMonitor) Close() error {
	m.closeOnce.Do(func() { m.closeErr = m.inner.Close(); m.finish(true) })
	return m.closeErr
}
