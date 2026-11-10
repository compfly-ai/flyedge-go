// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CompFly AI

package flyedge

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// This is the existing governed provider boundary, not framework instrumentation.
// Only an admitted invocation captures usage; ordinary requests remain unchanged.
func (t *guardRoundTripper) roundTripModel(req *http.Request, model string) (resp *http.Response, err error) {
	scope, _ := req.Context().Value(invocationUsageContextKey{}).(*invocationUsage)
	if scope == nil || scope.guard != t.guard {
		return t.base.RoundTrip(req)
	}
	callID := randID()
	if !scope.start(callID, model) {
		return t.base.RoundTrip(req)
	}
	monitored := false
	defer func() {
		if !monitored {
			scope.complete(req.Context(), callID, nil)
		}
	}()
	resp, err = t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, err
	}
	streamed := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	resp.Body = &responsesMonitor{inner: resp.Body, done: func(raw []byte, truncated, interrupted bool) {
		counts, incomplete := observedModelUsage(raw, streamed)
		if counts != nil && (truncated || interrupted || incomplete) {
			// Intermediate output does not establish a complete billed stream total.
			counts.OutputTokens, counts.ReasoningTokens, counts.TotalTokens = nil, nil, nil
		}
		scope.complete(req.Context(), callID, counts)
	}}
	monitored = true
	return resp, err
}

func knownUsageNumber(m map[string]any, keys ...string) (*uint64, bool) {
	for _, key := range keys {
		value, present := m[key]
		if !present || value == nil {
			continue
		}
		number, ok := value.(json.Number)
		if !ok {
			return nil, false
		}
		count, err := strconv.ParseUint(string(number), 10, 64)
		if err != nil || count > maxInvocationUsageCounter {
			return nil, false
		}
		return usageCount(count), true
	}
	return nil, true
}

func usageDetails(m map[string]any, names ...string) map[string]any {
	for _, name := range names {
		if details, ok := m[name].(map[string]any); ok {
			return details
		}
	}
	return nil
}

// These are the usage objects of the protocols already recognized by the wrap.
// Presence is retained: absent dimensions are never converted to measured zero.
func usageObject(event map[string]any) (*ModelUsage, bool) {
	for _, nested := range []string{"message", "response"} {
		if value, ok := event[nested].(map[string]any); ok {
			if usage, found := usageObject(value); found {
				return usage, true
			}
		}
	}
	u, found := event["usage"].(map[string]any)
	gemini := false
	if !found {
		u, found = event["usageMetadata"].(map[string]any)
		gemini = found
	}
	if !found {
		return nil, false
	}
	var result ModelUsage
	var ok bool
	if result.InputTokens, ok = knownUsageNumber(u, "input_tokens", "prompt_tokens", "promptTokenCount"); !ok {
		return nil, true
	}
	if result.OutputTokens, ok = knownUsageNumber(u, "output_tokens", "completion_tokens", "candidatesTokenCount"); !ok {
		return nil, true
	}
	if result.TotalTokens, ok = knownUsageNumber(u, "total_tokens", "totalTokenCount"); !ok {
		return nil, true
	}
	if result.CachedInputTokens, ok = knownUsageNumber(u, "cache_read_input_tokens", "cachedContentTokenCount"); !ok {
		return nil, true
	}
	if result.CachedInputTokens == nil {
		result.CachedInputTokens, ok = knownUsageNumber(usageDetails(u, "input_tokens_details", "prompt_tokens_details"), "cached_tokens")
		if !ok {
			return nil, true
		}
	}
	result.ReasoningTokens, ok = knownUsageNumber(usageDetails(u, "output_tokens_details", "completion_tokens_details"), "reasoning_tokens")
	if !ok {
		return nil, true
	}
	if gemini {
		result.ReasoningTokens, ok = knownUsageNumber(u, "thoughtsTokenCount")
		if !ok {
			return nil, true
		}
		// Gemini's candidates exclude thoughts. Shared usage output includes
		// reasoning, with its measured subset retained separately.
		if result.OutputTokens != nil && !addUsageCount(&result.OutputTokens, result.ReasoningTokens) {
			return nil, true
		}
	}
	// Anthropic reports uncached input separately from cache reads and creation.
	if result.InputTokens != nil {
		for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens"} {
			cache, valid := knownUsageNumber(u, key)
			if !valid || !addUsageCount(&result.InputTokens, cache) {
				return nil, true
			}
		}
	}
	return &result, true
}

func mergeUsageMax(total *ModelUsage, counts *ModelUsage) {
	for _, pair := range []struct {
		dst **uint64
		src *uint64
	}{
		{&total.InputTokens, counts.InputTokens}, {&total.CachedInputTokens, counts.CachedInputTokens},
		{&total.OutputTokens, counts.OutputTokens}, {&total.ReasoningTokens, counts.ReasoningTokens}, {&total.TotalTokens, counts.TotalTokens},
	} {
		if pair.src != nil && (*pair.dst == nil || *pair.src > **pair.dst) {
			*pair.dst = usageCount(*pair.src)
		}
	}
}

func usageEventState(event map[string]any) (terminal, incomplete bool) {
	if event["error"] != nil {
		incomplete = true
	}
	switch event["status"] {
	case "failed", "incomplete":
		incomplete = true
	}
	switch event["type"] {
	case "message_stop", "response.completed":
		terminal = true
	case "error", "response.failed", "response.incomplete":
		incomplete = true
	}
	if response, ok := event["response"].(map[string]any); ok {
		_, partial := usageEventState(response)
		incomplete = incomplete || partial
	}
	for _, field := range []string{"choices", "candidates"} {
		items, _ := event[field].([]any)
		for _, item := range items {
			if value, ok := item.(map[string]any); ok {
				if reason, ok := value["finish_reason"].(string); ok && reason != "" {
					terminal = true
				}
				if reason, ok := value["finishReason"].(string); ok && reason != "" {
					terminal = true
				}
			}
		}
	}
	return
}

func observedModelUsage(raw []byte, streamed bool) (*ModelUsage, bool) {
	decode := func(raw []byte) (map[string]any, bool) {
		var value map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil || value == nil {
			return nil, false
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return nil, false
		}
		return value, true
	}
	if !streamed {
		event, ok := decode(raw)
		if !ok {
			return nil, true
		}
		_, partial := usageEventState(event)
		// In-progress events are expected within a stream, but a standalone
		// response in this state cannot establish a completed model attempt.
		if status, _ := event["status"].(string); status == "queued" || status == "in_progress" {
			partial = true
		}
		usage, _ := usageObject(event)
		if usage != nil {
			copy := cloneModelUsage(usage)
			if !copy.valid() {
				return nil, true
			}
		}
		return usage, partial
	}
	var total ModelUsage
	found, terminal, incomplete := false, false, false
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		data, ok := strings.CutPrefix(strings.TrimSpace(string(line)), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			terminal = true
			continue
		}
		if data == "" {
			continue
		}
		event, ok := decode([]byte(data))
		if !ok {
			incomplete = true
			continue
		}
		ended, partial := usageEventState(event)
		terminal = terminal || ended
		incomplete = incomplete || partial
		counts, present := usageObject(event)
		if present && counts == nil {
			return nil, true
		}
		if counts != nil {
			found = true
			mergeUsageMax(&total, counts)
		}
	}
	if !found {
		return nil, true
	}
	copy := cloneModelUsage(&total)
	if !copy.valid() {
		return nil, true
	}
	return &total, incomplete || !terminal
}
