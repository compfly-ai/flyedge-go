package telemetry

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestBatchedFlushesOnClose(t *testing.T) {
	var mu sync.Mutex
	var batches [][]byte
	sender := func(_ context.Context, body []byte) error {
		mu.Lock()
		batches = append(batches, body)
		mu.Unlock()
		return nil
	}

	b := NewBatched(sender, "sess-1", time.Hour) // long interval → only Close flushes
	b.Record(Event{Stage: "pre_llm", Action: "allow", Model: "gpt-4o", LatencyMS: 12, SessionID: "sess-abc", RequestID: "req-1"})
	b.Record(Event{Stage: "tool_call", Action: "deny", Reason: "x"})

	// Report reflects both locally
	if s := b.Report(); s.Checks != 2 || s.Allowed != 1 || s.Denied != 1 {
		t.Fatalf("report = %+v", s)
	}

	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 1 {
		t.Fatalf("want 1 batch flushed on close, got %d", len(batches))
	}
	var batch telemetryBatch
	if err := json.Unmarshal(batches[0], &batch); err != nil {
		t.Fatalf("batch json: %v", err)
	}
	if batch.SessionID != "sess-1" || batch.EventCount != 2 || len(batch.Events) != 2 {
		t.Fatalf("batch = %+v", batch)
	}
	if batch.Events[0].Type != "protection_event" || batch.Events[0].Source != "sdk" || batch.Events[0].Model != "gpt-4o" {
		t.Errorf("event[0] = %+v", batch.Events[0])
	}
	// The real per-event session/request id must survive into the emitted event
	// so prism can correlate this check with the rest of the session's telemetry.
	if batch.Events[0].SessionID != "sess-abc" || batch.Events[0].RequestID != "req-1" {
		t.Errorf("event[0] correlation ids not carried: session=%q request=%q", batch.Events[0].SessionID, batch.Events[0].RequestID)
	}
}

func TestRichEventsShipButDontCountAsChecks(t *testing.T) {
	var mu sync.Mutex
	var batches [][]byte
	sender := func(_ context.Context, body []byte) error {
		mu.Lock()
		batches = append(batches, body)
		mu.Unlock()
		return nil
	}

	b := NewBatched(sender, "sess-x", time.Hour)
	b.Record(Event{Stage: "pre_llm", Action: "allow"})                                                     // a check
	b.Record(Event{Type: EventLLMIO, Model: "gpt-4o", Provider: "openai", InputTokens: 10, OutputTokens: 5}) // rich
	b.Record(Event{Type: EventToolIO, Name: "search", RequestFull: "{}"})                                    // rich

	// Only the policy check feeds the Summary.
	if s := b.Report(); s.Checks != 1 || s.Allowed != 1 {
		t.Fatalf("rich events should not count as checks: %+v", s)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var batch telemetryBatch
	if err := json.Unmarshal(batches[0], &batch); err != nil {
		t.Fatalf("batch json: %v", err)
	}
	// All three events ship, with prism-recognized types + rich fields carried.
	if batch.EventCount != 3 || len(batch.Events) != 3 {
		t.Fatalf("want 3 events shipped, got %+v", batch.EventCount)
	}
	byType := map[string]otelEvent{}
	for _, e := range batch.Events {
		byType[e.Type] = e
	}
	if _, ok := byType["protection_event"]; !ok {
		t.Errorf("missing protection_event; types=%v", batch.Events)
	}
	if llm := byType["llm_io"]; llm.Model != "gpt-4o" || llm.Provider != "openai" || llm.InputTokens != 10 || llm.OutputTokens != 5 || llm.TotalTokens != 0 {
		t.Errorf("llm_io fields not carried: %+v", llm)
	}
	if tool := byType["tool_io"]; tool.Name != "search" || tool.RequestFull != "{}" {
		t.Errorf("tool_io fields not carried: %+v", tool)
	}
}
