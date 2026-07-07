package flyedge

import (
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

// WithMode overrides Config.Mode.
func WithMode(m Mode) Option {
	return func(g *Guard) error { g.cfg.Mode = m; return nil }
}

// WithFailMode overrides Config.FailMode.
func WithFailMode(f FailMode) Option {
	return func(g *Guard) error { g.cfg.FailMode = f; return nil }
}
