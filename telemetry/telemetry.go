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

// Event is one policy-check outcome. Action is the normalized decision ("allow"/"deny"/"warn"),
// Err is set when the enforcement call itself failed.
type Event struct {
	Stage      string
	Action     string
	Reason     string
	Model      string
	LatencyMS  float64
	Err        string
	OccurredAt time.Time
}

// Summary is an aggregate view over recorded events — the value Guard.Report() returns.
type Summary struct {
	Checks    int
	Allowed   int
	Denied    int
	Warned    int
	Errors    int
	ByStage   map[string]int
	TotalMS   float64
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

func (Noop) Record(Event)     {}
func (Noop) Report() Summary  { return Summary{ByStage: map[string]int{}} }
func (Noop) Close() error     { return nil }

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
