// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"testing"
	"time"

	"github.com/compfly-ai/flyedge-go/telemetry"
)

type captureTelemetry struct {
	events []telemetry.Event
}

func (c *captureTelemetry) Record(ev telemetry.Event) {
	c.events = append(c.events, ev)
}

func (c *captureTelemetry) Report() telemetry.Summary {
	return telemetry.Summary{ByStage: map[string]int{}}
}

func (c *captureTelemetry) Close() error {
	return nil
}

func TestRecordToolIODetailCarriesEndpointAttribution(t *testing.T) {
	tel := &captureTelemetry{}
	g := &Guard{tel: tel}

	g.RecordToolIODetail(ToolIO{
		SessionID: "sess-1", RequestID: "tool-1", ToolName: "Bash",
		EndpointID: "endpoint-1", InstanceKey: "claude-code:/repo",
		AgentFramework: "flyedged-hooks",
	})

	if len(tel.events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(tel.events))
	}
	got := tel.events[0]
	if got.Type != telemetry.EventToolIO || got.Name != "Bash" || got.Operation != "tool.call" {
		t.Fatalf("unexpected event shape: %+v", got)
	}
	if got.EndpointID != "endpoint-1" || got.InstanceKey != "claude-code:/repo" {
		t.Fatalf("endpoint attribution not carried: %+v", got)
	}
	if got.AgentFramework != "flyedged-hooks" {
		t.Fatalf("agent framework not carried: %+v", got)
	}
}

// An observed LLM call has to be placeable in the turn it belongs to: without trace/span ids the
// call never joins the trace its own policy checks are on, so a lifecycle view shows the checks and
// none of the operations they governed.
func TestRecordLLMCallDetailCarriesTracePlacement(t *testing.T) {
	tel := &captureTelemetry{}
	g := &Guard{tel: tel}
	when := time.Date(2026, 9, 8, 16, 42, 0, 394_000_000, time.UTC)

	g.RecordLLMCallDetail(LLMCall{
		SessionID:  "sess-1",
		Model:      "claude-fable-5-1",
		TraceID:    "0123456789abcdef0123456789abcdef",
		SpanID:     "fedcba9876543210",
		OccurredAt: when,
	})

	if len(tel.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(tel.events))
	}
	ev := tel.events[0]
	if ev.TraceID != "0123456789abcdef0123456789abcdef" || ev.SpanID != "fedcba9876543210" {
		t.Errorf("trace placement not carried: trace=%q span=%q", ev.TraceID, ev.SpanID)
	}
	if !ev.OccurredAt.Equal(when) {
		t.Errorf("OccurredAt = %s, want the observed instant %s", ev.OccurredAt, when)
	}
}

// A subagent id stands in for a span only when the caller named none. It is an agent id, not a
// 16-hex span, so letting it win would silently undo the placement it was passed for.
func TestRecordLLMCallDetailSpanBeatsSubagentID(t *testing.T) {
	tel := &captureTelemetry{}
	g := &Guard{tel: tel}

	g.RecordLLMCallDetail(LLMCall{SessionID: "sess-1", AgentID: "agent-7", SpanID: "fedcba9876543210"})
	g.RecordLLMCallDetail(LLMCall{SessionID: "sess-1", AgentID: "agent-7"})

	if len(tel.events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(tel.events))
	}
	if got := tel.events[0].SpanID; got != "fedcba9876543210" {
		t.Errorf("explicit span must win over the subagent id, got %q", got)
	}
	if got := tel.events[1].SpanID; got != "agent-7" {
		t.Errorf("with no span the subagent id still stands in, got %q", got)
	}
	// Delegation is still reported either way.
	for i, ev := range tel.events {
		if ev.Data["delegated"] != true || ev.Data["subagent_id"] != "agent-7" {
			t.Errorf("event %d lost its delegation payload: %+v", i, ev.Data)
		}
	}
}

// A zero OccurredAt means "now" — an inline caller does not have to supply a clock.
func TestRecordEventsDefaultOccurredAtToNow(t *testing.T) {
	tel := &captureTelemetry{}
	g := &Guard{tel: tel}
	before := time.Now()

	g.RecordLLMCallDetail(LLMCall{SessionID: "sess-1"})
	g.RecordToolIODetail(ToolIO{SessionID: "sess-1", ToolName: "Bash"})

	for i, ev := range tel.events {
		if ev.OccurredAt.Before(before) {
			t.Errorf("event %d: OccurredAt %s predates the call", i, ev.OccurredAt)
		}
	}
}

// A tool call observed by a sensor carries the same placement facts as an LLM call.
func TestRecordToolIODetailCarriesTracePlacementAndTime(t *testing.T) {
	tel := &captureTelemetry{}
	g := &Guard{tel: tel}
	when := time.Date(2026, 9, 8, 16, 42, 41, 479_000_000, time.UTC)

	g.RecordToolIODetail(ToolIO{
		SessionID:  "sess-1",
		ToolName:   "Bash",
		TraceID:    "0123456789abcdef0123456789abcdef",
		SpanID:     "1111222233334444",
		OccurredAt: when,
	})

	ev := tel.events[0]
	if ev.TraceID != "0123456789abcdef0123456789abcdef" || ev.SpanID != "1111222233334444" {
		t.Errorf("trace placement not carried: trace=%q span=%q", ev.TraceID, ev.SpanID)
	}
	if !ev.OccurredAt.Equal(when) {
		t.Errorf("OccurredAt = %s, want %s", ev.OccurredAt, when)
	}
}

func TestRecordLLMCallDetailCarriesContent(t *testing.T) {
	tel := &captureTelemetry{}
	g := &Guard{tel: tel}
	g.RecordLLMCallDetail(LLMCall{SessionID: "s", RequestFull: "Fix the bug", ResponseFull: "Fixed it."})
	if len(tel.events) != 1 || tel.events[0].RequestFull != "Fix the bug" || tel.events[0].ResponseFull != "Fixed it." {
		t.Fatalf("content lost: %+v", tel.events)
	}
}
