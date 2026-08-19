// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Package edgesync is the generic poll/report rails behind every edge sync channel — edge packs,
// local controls, and whatever comes after. Before this package, config_poll.go's heartbeat loop
// was the only precedent, and it is entirely Guard-specific (one hardcoded path, one fixed
// response struct, fields owned directly by Guard) — not something a second, unrelated sync
// channel could reuse without copying its shape wholesale. Syncer factors that shape out: a signed
// poll on an interval, change detection by content hash, a callback fired only on real change, and
// an optional signed report call — so a second sync channel is a few lines of wiring, not a copy
// of the first.
//
// Syncer does not know or care what it is syncing. It moves bytes; the caller (edge-pack sync,
// local-controls sync) owns decoding, converging local state, and deciding what to report.
package edgesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// Transport is the signed HTTP capability a Syncer needs. *enforce.HTTPEnforcer already
// implements this (GetSigned/PostSigned are exported precisely because config_poll.go's
// signedGetter/connect.go's signedPoster seams needed them) — no new signing code, no new
// dependency. A test stub can substitute a fake satisfying the same two methods.
type Transport interface {
	GetSigned(ctx context.Context, path string, headers map[string]string) ([]byte, error)
	PostSigned(ctx context.Context, path string, body []byte) ([]byte, error)
}

// conditionalGetter is an OPTIONAL Transport capability: a signed GET that reports a 304 Not
// Modified as an ordinary outcome instead of an error. When the Transport implements it, Poll
// sends If-None-Match with the last response's hash and a server that recognizes it can answer
// "nothing changed" with an empty body — which is the steady state, since distributed config
// changes far less often than it is polled. Transports without it (test stubs, older
// implementations) fall back to a plain GET and the same content-hash comparison, so behavior is
// identical, just chattier.
type conditionalGetter interface {
	GetSignedConditional(ctx context.Context, path string, headers map[string]string) ([]byte, bool, error)
}

// headerIfNoneMatch carries the last-seen response hash on a conditional poll. The value is this
// package's own content hash (hex sha256 of the exact response bytes), not an opaque server
// token — the server computes the same hash over what it is about to send and compares.
const headerIfNoneMatch = "If-None-Match"

// Syncer polls a signed endpoint on an interval and invokes OnUpdate only when the raw response
// content actually changes (sha256 comparison) — a healthy steady-state poll that returns
// unchanged bytes is silent. It can also report local state back over a second signed endpoint,
// either per-tick (WithReportBuilder, mirrors today's "the pull doubles as the report" cadence)
// or on demand (Report, for reporting immediately after a local convergence rather than waiting
// for the next tick).
//
// Poll errors are non-fatal — this is config distribution, not enforcement; a failed tick just
// retries next interval, the same philosophy as the existing heartbeat poller.
type Syncer struct {
	transport  Transport
	pollPath   string
	reportPath string
	interval   time.Duration
	headers    func() map[string]string
	onUpdate   func(raw []byte)
	onReport   func() ([]byte, error)

	mu       sync.Mutex
	lastHash string
	started  bool
	stop     chan struct{}
	done     chan struct{}
}

// Option configures a Syncer at construction.
type Option func(*Syncer)

// WithReportPath sets the signed POST endpoint Report (and, if WithReportBuilder is also set, the
// per-tick auto-report) targets. A Syncer with no report path is poll-only — Report returns an
// error, and a report builder is never invoked.
func WithReportPath(path string) Option {
	return func(s *Syncer) { s.reportPath = path }
}

// WithHeaders attaches extra headers (e.g. hostname, a locally-tracked hash) to every poll GET,
// mirroring config_poll.go's heartbeatHeaders.
func WithHeaders(fn func() map[string]string) Option {
	return func(s *Syncer) { s.headers = fn }
}

// WithOnUpdate sets the callback fired with the raw response body whenever a poll's content
// differs from the last one seen. Optional — a Syncer with no OnUpdate is a poll-and-discard
// heartbeat (rarely useful) or, combined with WithReportBuilder alone, a report-only channel.
func WithOnUpdate(fn func(raw []byte)) Option {
	return func(s *Syncer) { s.onUpdate = fn }
}

// WithReportBuilder causes the poll loop to also report on every tick, regardless of whether the
// poll response changed: after each successful poll, onReport builds the current local state and
// Syncer POSTs it to the report path. Errors from onReport or the POST are swallowed — same
// best-effort posture as the poll itself. Requires WithReportPath.
func WithReportBuilder(fn func() ([]byte, error)) Option {
	return func(s *Syncer) { s.onReport = fn }
}

// New builds a Syncer. pollPath is the signed GET endpoint to watch for change; interval is the
// poll cadence. This package imposes no minimum interval — callers own that policy (flyedged's
// FLYEDGED_PACK_INTERVAL floor, for example).
func New(transport Transport, pollPath string, interval time.Duration, opts ...Option) *Syncer {
	s := &Syncer{
		transport: transport,
		pollPath:  pollPath,
		interval:  interval,
		headers:   func() map[string]string { return nil },
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Start launches the poll loop in its own goroutine (idempotent — a second Start on an already-
// running Syncer is a no-op). Polls immediately so state is fresh without waiting a full interval,
// then on the configured tick.
func (s *Syncer) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	stop, done := s.stop, s.done
	s.mu.Unlock()

	go func() {
		defer close(done)
		t := time.NewTicker(s.interval)
		defer t.Stop()
		s.tick()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.tick()
			}
		}
	}()
}

// Stop signals the poll loop to exit and blocks until it has. Safe to call on a Syncer that was
// never Started.
func (s *Syncer) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	stop, done := s.stop, s.done
	s.started = false
	s.mu.Unlock()
	close(stop)
	<-done
}

// Poll does one signed GET and reports whether the content changed since the last call this
// Syncer made (sha256 of the raw bytes). Exported so a caller wanting manual control — a one-shot
// check rather than a background loop — never needs Start/Stop at all.
//
// When the Transport supports conditional GETs, the last-seen hash goes out as If-None-Match and
// an unchanged response comes back as a bodiless 304 — so a steady-state poll transfers nothing.
// A 304 returns (nil, false, nil): no bytes and no change, which is exactly what the caller
// already handles, since it only acts on changed == true.
func (s *Syncer) Poll(ctx context.Context) (raw []byte, changed bool, err error) {
	s.mu.Lock()
	prev := s.lastHash
	s.mu.Unlock()

	headers := s.headers()
	if cg, ok := s.transport.(conditionalGetter); ok {
		if prev != "" {
			if headers == nil {
				headers = map[string]string{}
			}
			headers[headerIfNoneMatch] = prev
		}
		raw, notModified, err := cg.GetSignedConditional(ctx, s.pollPath, headers)
		if err != nil {
			return nil, false, err
		}
		if notModified {
			return nil, false, nil
		}
		h := contentHash(raw)
		s.mu.Lock()
		changed = h != s.lastHash
		s.lastHash = h
		s.mu.Unlock()
		return raw, changed, nil
	}

	raw, err = s.transport.GetSigned(ctx, s.pollPath, headers)
	if err != nil {
		return nil, false, err
	}
	h := contentHash(raw)
	s.mu.Lock()
	changed = h != s.lastHash
	s.lastHash = h
	s.mu.Unlock()
	return raw, changed, nil
}

// Report signs and POSTs body to the configured report path. Returns an error if no report path
// was set via WithReportPath — reporting is opt-in per channel.
func (s *Syncer) Report(ctx context.Context, body []byte) error {
	if s.reportPath == "" {
		return fmt.Errorf("edgesync: no report path configured (WithReportPath)")
	}
	_, err := s.transport.PostSigned(ctx, s.reportPath, body)
	return err
}

// tick runs one poll-and-react cycle plus, if configured, one auto-report. Called both for the
// immediate poll on Start and every subsequent interval.
func (s *Syncer) tick() {
	raw, changed, err := s.Poll(context.Background())
	if err == nil && changed && s.onUpdate != nil {
		s.onUpdate(raw)
	}
	if s.onReport != nil && s.reportPath != "" {
		if body, err := s.onReport(); err == nil {
			_ = s.Report(context.Background(), body)
		}
	}
}

func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}