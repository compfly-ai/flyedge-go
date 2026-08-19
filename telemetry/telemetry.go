// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Package telemetry defines the flyedge protection-telemetry seam. It is explicit and injectable:
// the Guard records an Event per policy check into whatever Telemetry you install, and Report()
// returns a Summary you print if and when you want — the deliberate replacement for the Python
// SDK's auto-printed "PROTECTION SUMMARY". The default (in-memory Recorder) needs no wiring; a
// cloud/OTel sink is opt-in.
package telemetry

import (
	"fmt"
	"sync"
	"time"
)

// Event types prism's telemetry handler recognizes (event_type → activity_type).
// An empty Type is treated as EventProtection (a policy check).
const (
	EventProtection     = "protection_event" // a policy-check outcome (allow/deny/warn)
	EventLLMIO          = "llm_io"           // a model call: model/provider/tokens/latency (+audit)
	EventToolIO         = "tool_io"          // a tool call: name/args/result
	EventSessionStart   = "session_start"    // agent session opened
	EventSessionSummary = "session_summary"  // agent session ended (rolled-up stats in Data)
)

// Event is one telemetry record. For a policy check (the default — Type=="" or
// EventProtection) Action is the normalized decision and Err is set when the
// enforcement call itself failed. Rich types (llm_io/tool_io/session_*) carry the
// model/tool/session fields below and are NOT counted as checks in Summary.
type Event struct {
	Type      string // event_type; "" ⇒ protection_event
	Stage     string
	Action    string
	Reason    string
	Model     string
	Provider  string
	Operation string
	Name      string // span / tool name
	LatencyMS float64
	// InputTokens is the provider-reported UNCACHED input count. Cache reads/writes are
	// reported separately below and are also input tokens — providers split them because they
	// price differently, not because the model didn't process them. The wire carries the sum;
	// see toWire.
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	// Cache token counts, when the provider reports them (Anthropic prompt caching). These
	// dominate real usage for long-running coding sessions: a turn commonly shows 2 uncached
	// input tokens against ~380k cache reads, so omitting them understates input by orders of
	// magnitude.
	CacheReadTokens  int64
	CacheWriteTokens int64
	Streaming        *bool // set on streamed model calls
	Err              string
	OccurredAt       time.Time
	// SessionID / RequestID correlate this record with the agent's other telemetry
	// and prism's /check record for the same session. Sourced from CheckRequest.
	SessionID string
	RequestID string
	// EndpointID / InstanceKey attribute this record to the endpoint-agent instance that
	// produced it — the durable device and the (agent, repository) identity a sensor resolves.
	// Both empty for a plain agent call; the platform joins on them when present.
	EndpointID  string
	InstanceKey string
	// TraceID / SpanID / ParentSpanID place this record in prism's lifecycle span
	// tree (W3C ids). Empty ⇒ prism treats it as unparented.
	TraceID      string
	SpanID       string
	ParentSpanID string
	// AgentFramework + audit payloads + arbitrary Data (session/protection events).
	AgentFramework string
	RequestFull    string
	ResponseFull   string
	Data           map[string]any
}

// isCheck reports whether this event is a policy check (feeds the check Summary).
func (e Event) isCheck() bool { return e.Type == "" || e.Type == EventProtection }

// Summary is an aggregate view over recorded events — the value Guard.Report() returns.
type Summary struct {
	Checks  int
	Allowed int
	Denied  int
	Warned  int
	Errors  int
	ByStage map[string]int
	TotalMS float64
}

func (s Summary) String() string {
	return fmt.Sprintf("flyedge: %d checks — %d allowed, %d denied, %d warned, %d errors (%.1fms total)",
		s.Checks, s.Allowed, s.Denied, s.Warned, s.Errors, s.TotalMS)
}

// Telemetry receives protection events and can summarize them. Implementations must be safe for
// concurrent Record calls. Close flushes and releases any owned resources (goroutines).
type Telemetry interface {
	Record(ev Event)
	Report() Summary
	Close() error
}

// Noop discards events and reports an empty Summary.
type Noop struct{}

func (Noop) Record(Event)    {}
func (Noop) Report() Summary { return Summary{ByStage: map[string]int{}} }
func (Noop) Close() error    { return nil }

// Recorder is the default in-memory telemetry: thread-safe aggregation, no I/O. Report reflects
// everything recorded so far.
type Recorder struct {
	mu  sync.Mutex
	sum Summary
}

// NewRecorder returns an empty in-memory Recorder.
func NewRecorder() *Recorder {
	return &Recorder{sum: Summary{ByStage: map[string]int{}}}
}

func (r *Recorder) Record(ev Event) {
	// Only policy checks feed the check Summary; rich event types (llm_io/tool_io/
	// session_*) are shipped by cloud sinks but must not inflate the check counters.
	if !ev.isCheck() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sum.Checks++
	r.sum.TotalMS += ev.LatencyMS
	r.sum.ByStage[ev.Stage]++
	switch {
	case ev.Err != "":
		r.sum.Errors++
	case ev.Action == "deny":
		r.sum.Denied++
	case ev.Action == "warn":
		r.sum.Warned++
	default:
		r.sum.Allowed++
	}
}

// Report returns a copy of the current aggregate (safe to read/print concurrently with Record).
func (r *Recorder) Report() Summary {
	r.mu.Lock()
	defer r.mu.Unlock()
	byStage := make(map[string]int, len(r.sum.ByStage))
	for k, v := range r.sum.ByStage {
		byStage[k] = v
	}
	cp := r.sum
	cp.ByStage = byStage
	return cp
}

func (r *Recorder) Close() error { return nil }
