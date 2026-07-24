package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Sender ships a serialized FlyedgeTelemetryBatch to the gateway (/v1/flyedge/telemetry). The Guard
// wires one backed by a signed POST; tests inject a fake.
type Sender func(ctx context.Context, batchJSON []byte) error

// Batched is a cloud telemetry sink: it records events locally (so Report still works) AND buffers
// them for periodic delivery to the gateway on an OWNED goroutine, flushed on Close. This is the
// explicit lifecycle the Python SDK does with fire-and-forget daemon threads — here it's owned and
// stopped deterministically.
type Batched struct {
	rec      *Recorder
	sender   Sender
	session  string
	interval time.Duration

	mu        sync.Mutex
	buf       []Event
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// NewBatched starts a batcher that flushes every interval (min 1s). Call Close to flush + stop.
func NewBatched(sender Sender, session string, interval time.Duration) *Batched {
	if interval < time.Second {
		interval = 5 * time.Second
	}
	b := &Batched{
		rec: NewRecorder(), sender: sender, session: session, interval: interval,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go b.loop()
	return b
}

func (b *Batched) Record(ev Event) {
	b.rec.Record(ev)
	b.mu.Lock()
	b.buf = append(b.buf, ev)
	b.mu.Unlock()
}

func (b *Batched) Report() Summary { return b.rec.Report() }

// Close stops the flush goroutine and flushes any buffered events one last time. Idempotent —
// safe to call multiple times (e.g. an explicit Close plus a deferred one).
func (b *Batched) Close() error {
	var err error
	b.closeOnce.Do(func() {
		close(b.stop)
		<-b.done
		err = b.flush(context.Background())
	})
	return err
}

func (b *Batched) loop() {
	defer close(b.done)
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = b.flush(ctx)
			cancel()
		}
	}
}

// flush drains the buffer and ships it as one batch. Errors are swallowed (telemetry is
// best-effort and must never break the agent) but the events are dropped, not retried, to bound memory.
func (b *Batched) flush(ctx context.Context) error {
	b.mu.Lock()
	if len(b.buf) == 0 {
		b.mu.Unlock()
		return nil
	}
	events := b.buf
	b.buf = nil
	b.mu.Unlock()

	batch := telemetryBatch{
		BatchID:     "batch-" + randHex(),
		SessionID:   b.session,
		TimestampMS: time.Now().UnixMilli(),
		EventCount:  len(events),
		Events:      toOtelEvents(events),
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	return b.sender(ctx, body)
}

// --- wire shapes for /v1/flyedge/telemetry (match prism FlyedgeTelemetryBatch / FlyedgeOtelEvent) ---

type telemetryBatch struct {
	BatchID     string      `json:"batch_id"`
	SessionID   string      `json:"session_id"`
	TimestampMS int64       `json:"timestamp_ms"`
	EventCount  int         `json:"event_count"`
	Events      []otelEvent `json:"events"`
}

type otelEvent struct {
	Type      string `json:"type"`
	Source    string `json:"source"`
	RequestID string `json:"request_id,omitempty"`
	Model     string `json:"model,omitempty"`
	LatencyMS uint64 `json:"latency_ms,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Timestamp string `json:"timestamp"`
	Data      any    `json:"data,omitempty"`
}

func toOtelEvents(evs []Event) []otelEvent {
	out := make([]otelEvent, 0, len(evs))
	for _, e := range evs {
		out = append(out, otelEvent{
			// "protection_event" is the type prism's telemetry handler recognizes
			// (maps to activity_type=protection_event). "flyedge_check" fell through
			// the handler's `other => other` arm as an unmapped activity_type.
			Type:      "protection_event",
			Source:    "sdk",
			RequestID: e.RequestID,
			Model:     e.Model,
			LatencyMS: uint64(e.LatencyMS),
			SessionID: e.SessionID,
			Timestamp: e.OccurredAt.UTC().Format(time.RFC3339),
			Data:      map[string]any{"stage": e.Stage, "action": e.Action, "reason": e.Reason, "error": e.Err},
		})
	}
	return out
}

func randHex() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
