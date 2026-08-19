// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"time"

	"github.com/compfly-ai/flyedge-go/enforce"
	"github.com/compfly-ai/flyedge-go/identity"
	"github.com/compfly-ai/flyedge-go/telemetry"
)

// Option customizes a Guard at construction. Options make the seams explicit and testable: inject
// a fake Enforcer for offline tests, or a custom Signer (KMS, agent runtime) without env plumbing.
type Option func(*Guard) error

func applyOptions(g *Guard, opts []Option) error {
	for _, opt := range opts {
		if err := opt(g); err != nil {
			return err
		}
	}
	return nil
}

// WithSigner injects a Signer, overriding the one New would build from Config. Pass nil explicitly
// to run unsigned.
func WithSigner(s identity.Signer) Option {
	return func(g *Guard) error { g.signer = s; return nil }
}

// WithEnforcer injects the policy decision point — e.g. a stub in tests, or an offline/record
// implementation. Overrides the default HTTP enforcer.
func WithEnforcer(e enforce.Enforcer) Option {
	return func(g *Guard) error { g.enforcer = e; return nil }
}

// WithTelemetry injects the telemetry sink (e.g. Noop, a cloud batcher, or an OTel bridge),
// overriding the default in-memory Recorder.
func WithTelemetry(t telemetry.Telemetry) Option {
	return func(g *Guard) error { g.tel = t; return nil }
}

// WithCloudTelemetry ships protection events to the gateway (/v1/flyedge/telemetry) via a batched,
// owned-goroutine sink (flushed on Close), in addition to keeping Report() working locally.
// interval is the flush cadence (0 → 5s). Requires the default signed HTTP enforcer.
func WithCloudTelemetry(interval time.Duration) Option {
	return func(g *Guard) error {
		g.cloudTelemetry = true
		g.telInterval = interval
		return nil
	}
}

// WithMode overrides Config.Mode.
func WithMode(m Mode) Option {
	return func(g *Guard) error { g.cfg.Mode = m; return nil }
}

// WithFailMode overrides Config.FailMode.
func WithFailMode(f FailMode) Option {
	return func(g *Guard) error { g.cfg.FailMode = f; return nil }
}

// WithHeartbeat overrides the config-poll interval. By default the cadence comes from the
// ConnectResponse (heartbeat_interval_seconds), falling back to 30s. Set a shorter interval to pick
// up model-mode / simulation changes faster (e.g. during local testing).
func WithHeartbeat(interval time.Duration) Option {
	return func(g *Guard) error { g.pollInterval = interval; return nil }
}

// WithModeChangeHandler registers a callback fired when the poller observes a change to the agent's
// model_mode (check/passthrough/gateway). Useful for logging or reacting to a mode flip.
func WithModeChangeHandler(fn func(old, cur ModelMode)) Option {
	return func(g *Guard) error { g.onModeChange = fn; return nil }
}

// WithManifestRefreshHandler overrides what happens when prism sets manifest_refresh_required. The
// default is to re-send the manifest (reconnect); provide a handler to customize (e.g. rebuild the
// manifest from live introspection first).
func WithManifestRefreshHandler(fn func()) Option {
	return func(g *Guard) error { g.onManifestRefresh = fn; return nil }
}

// WithSimulationTelemetryURL overrides the telemetry WebSocket URL the gateway advertises in the
// simulation config. Server-authoritative by default; set this only for split-horizon local dev
// (agent on the host, gateway in-cluster) so the controller dials a host-reachable URL instead of
// the in-cluster one. Equivalent to Config.SimTelemetryURL / COMPFLY_SIM_TELEMETRY_URL.
func WithSimulationTelemetryURL(url string) Option {
	return func(g *Guard) error { g.cfg.SimTelemetryURL = url; return nil }
}

// WithSimulation enables or disables the simulation client (default: enabled). When disabled, the
// agent will not act as a simulation / eval target even if the platform starts a run against it —
// the config poller still tracks the simulation block, but no telemetry is streamed and
// protection is never bypassed.
func WithSimulation(enabled bool) Option {
	return func(g *Guard) error { g.simEnabled = enabled; return nil }
}
