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
	b.Record(Event{Stage: "pre_llm", Action: "allow", Model: "gpt-4o", LatencyMS: 12})
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
	if batch.Events[0].Type != "flyedge_check" || batch.Events[0].Source != "sdk" || batch.Events[0].Model != "gpt-4o" {
		t.Errorf("event[0] = %+v", batch.Events[0])
	}
}
