// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/compfly-ai/flyedge-go/edgesync"
	"github.com/compfly-ai/flyedge-go/localcontrol"
)

// The local-controls sync channel: the platform publishes a rule set, every agent converges on it
// without a redeploy.
//
// This is the second instantiation of the edgesync rails (edge packs was the first), which is the
// whole reason those rails were factored out — a sync channel should be wiring, not a transport.

// LocalControlPollPath is prism's distribution endpoint for the org's local-control rule set.
const LocalControlPollPath = "/v1/flyedge/local-controls"

// LocalControlReportPath is where an agent reports the rule set it actually applied, so an
// operator can see convergence rather than assuming it.
const LocalControlReportPath = "/v1/flyedge/local-controls/report"

// DefaultLocalControlInterval is the poll cadence. Config distribution changes far less often than
// it is polled, and the conditional GET makes an unchanged tick nearly free, so this is chosen for
// "a rule change reaches the fleet promptly" rather than to limit bandwidth.
const DefaultLocalControlInterval = 5 * time.Minute

// localControlState is what the agent last successfully applied, reported back on each tick.
// Kept separate from the engine because a rejected config must not overwrite the record of what is
// actually running.
type localControlState struct {
	version   atomic.Int64
	appliedAt atomic.Pointer[time.Time]
	lastError atomic.Pointer[string]
}

// SyncLocalControls starts the local-controls sync channel on this Guard.
//
// The Guard's enforcer must be a signed HTTP enforcer (the default from New with a configured
// key) — the poll and report are both sensor/agent-signed. Returns an error if the transport
// cannot sign, rather than silently running an unauthenticated, and therefore unserved, loop.
//
// Stop it with StopLocalControlSync, or leave it to Close.
func (g *Guard) SyncLocalControls(opts ...LocalControlSyncOption) error {
	transport, ok := g.enforcer.(edgesync.Transport)
	if !ok {
		return fmt.Errorf("flyedge: local-control sync needs a signed HTTP enforcer; got %T", g.enforcer)
	}

	settings := localControlSyncSettings{interval: DefaultLocalControlInterval}
	for _, o := range opts {
		o(&settings)
	}

	g.lcSyncMu.Lock()
	defer g.lcSyncMu.Unlock()
	if g.lcSyncer != nil {
		return fmt.Errorf("flyedge: local-control sync already started")
	}

	syncer := edgesync.New(transport, LocalControlPollPath, settings.interval,
		edgesync.WithReportPath(LocalControlReportPath),
		edgesync.WithOnUpdate(func(raw []byte) { g.applyLocalControlPayload(raw, settings.onApply) }),
		edgesync.WithReportBuilder(g.buildLocalControlReport),
	)
	syncer.Start()
	g.lcSyncer = syncer
	return nil
}

// StopLocalControlSync stops the channel. Safe to call when it was never started.
func (g *Guard) StopLocalControlSync() {
	g.lcSyncMu.Lock()
	s := g.lcSyncer
	g.lcSyncer = nil
	g.lcSyncMu.Unlock()
	if s != nil {
		s.Stop()
	}
}

// applyLocalControlPayload decodes a published rule set and swaps it in.
//
// Every failure path here keeps the engine that is already running. A malformed or uncompilable
// payload is a publishing bug, and the correct response to one is to keep enforcing yesterday's
// known-good rules while reporting the failure — not to fall back to no protection, which is what
// clearing the engine would mean.
func (g *Guard) applyLocalControlPayload(raw []byte, onApply func(localcontrol.Config, error)) {
	var cfg localcontrol.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		g.recordLocalControlError(fmt.Errorf("decode: %w", err))
		if onApply != nil {
			onApply(cfg, err)
		}
		return
	}

	if err := g.SetLocalControls(cfg); err != nil {
		g.recordLocalControlError(err)
		if onApply != nil {
			onApply(cfg, err)
		}
		return
	}

	now := time.Now()
	g.lcState.version.Store(int64(cfg.Version))
	g.lcState.appliedAt.Store(&now)
	g.lcState.lastError.Store(nil)
	if onApply != nil {
		onApply(cfg, nil)
	}
}

func (g *Guard) recordLocalControlError(err error) {
	msg := err.Error()
	g.lcState.lastError.Store(&msg)
}

// localControlReport is what the agent tells the platform about its own convergence. The error
// field is reported rather than swallowed: an agent stuck on an old rule set because the new one
// will not compile is exactly the thing an operator needs to see, and it is invisible from the
// server side otherwise.
type localControlReport struct {
	Version   int      `json:"version"`
	Mode      string   `json:"mode"`
	Detectors []string `json:"detectors"`
	AppliedAt string   `json:"appliedAt,omitempty"`
	Error     string   `json:"error,omitempty"`
}

func (g *Guard) buildLocalControlReport() ([]byte, error) {
	rep := localControlReport{
		Version:   int(g.lcState.version.Load()),
		Mode:      string(g.local.load().Mode()),
		Detectors: g.LocalControlDetectors(),
	}
	if at := g.lcState.appliedAt.Load(); at != nil {
		rep.AppliedAt = at.UTC().Format(time.RFC3339)
	}
	if e := g.lcState.lastError.Load(); e != nil {
		rep.Error = *e
	}
	return json.Marshal(rep)
}

// LocalControlSyncOption customizes the sync channel.
type LocalControlSyncOption func(*localControlSyncSettings)

type localControlSyncSettings struct {
	interval time.Duration
	onApply  func(localcontrol.Config, error)
}

// WithLocalControlInterval overrides the poll cadence.
func WithLocalControlInterval(d time.Duration) LocalControlSyncOption {
	return func(s *localControlSyncSettings) {
		if d > 0 {
			s.interval = d
		}
	}
}

// WithLocalControlApplyHook observes each applied (or rejected) rule set. The SDK has no logger of
// its own, so this is how a host surfaces "the published rules did not compile" — without it, that
// failure is only visible in the report the platform receives.
func WithLocalControlApplyHook(fn func(cfg localcontrol.Config, err error)) LocalControlSyncOption {
	return func(s *localControlSyncSettings) { s.onApply = fn }
}