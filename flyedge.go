// Package flyedge is a Go agent-protection SDK, wire-compatible with the prism/policy-enforcer
// gateway. It is deliberately explicit ("gothonic"): construct a *Guard, pass it, and route calls
// through Guard.Check — no import-time monkeypatching, no ambient singletons, and a policy denial
// is a typed value (*DenyError), not an exception or a synthesized message. See DESIGN.md.
package flyedge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/compfly-ai/flyedge-go/enforce"
	"github.com/compfly-ai/flyedge-go/identity"
	"github.com/compfly-ai/flyedge-go/telemetry"
)

// Re-export the wire types so callers depend only on the flyedge package for the common surface.
type (
	Stage        = enforce.Stage
	Action       = enforce.Action
	Decision     = enforce.Decision
	CheckRequest = enforce.CheckRequest
	Content      = enforce.Content
	Operation    = enforce.Operation
)

const (
	StagePreLLM           = enforce.StagePreLLM
	StageToolCall         = enforce.StageToolCall
	StageToolCallResponse = enforce.StageToolCallResponse
	StagePostLLM          = enforce.StagePostLLM

	ActionAllow = enforce.ActionAllow
	ActionDeny  = enforce.ActionDeny
	ActionWarn  = enforce.ActionWarn
)

// Summary is the aggregate protection report (see Guard.Report).
type Summary = telemetry.Summary

// DenyError is returned by Check when the policy decision is a denial. Callers handle it as a
// value — errors.As(err, &de) — instead of catching an exception or inspecting a fake message.
type DenyError struct {
	Decision Decision
}

func (e *DenyError) Error() string {
	if e.Decision.Reason != "" {
		return "flyedge: denied: " + e.Decision.Reason
	}
	return "flyedge: denied"
}

// KillInfo describes an active kill switch (re-exported from enforce).
type KillInfo = enforce.KillInfo

// KillSwitchError is returned when a request is blocked by an operator kill switch — distinct from
// a policy DenyError because a kill ALWAYS enforces, bypassing FailMode (a kill can never be
// fail-open'd through). Kills carries the matching kill switch(es).
type KillSwitchError struct {
	Kills []KillInfo
}

func (e *KillSwitchError) Error() string {
	if len(e.Kills) > 0 {
		return "flyedge: kill switch active: " + e.Kills[0].Reason
	}
	return "flyedge: kill switch active"
}

// AsKillSwitchError reports whether err is a *KillSwitchError.
func AsKillSwitchError(err error) (*KillSwitchError, bool) {
	var ke *KillSwitchError
	if errors.As(err, &ke) {
		return ke, true
	}
	return nil, false
}

// Guard is the protection handle. Construct once with New, pass it explicitly, Close when done.
type Guard struct {
	cfg            Config
	signer         identity.Signer
	enforcer       enforce.Enforcer
	tel            telemetry.Telemetry
	cloudTelemetry bool          // WithCloudTelemetry: ship events to /v1/flyedge/telemetry
	telInterval    time.Duration // flush interval for cloud telemetry
}

// New builds a Guard from cfg (+ options). If a key is configured it builds a Signer; otherwise the
// guard runs check-only/unsigned (useful for tests/offline). Returns an error on invalid config —
// it never silently degrades. Importing this package has no side effects; nothing starts until New.
func New(cfg Config, opts ...Option) (*Guard, error) {
	cfg = cfg.withDefaults()
	g := &Guard{cfg: cfg}

	if err := applyOptions(g, opts); err != nil {
		return nil, err
	}

	// Build a signer from config unless one was injected via an option.
	if g.signer == nil && hasKey(cfg) {
		s, err := newSignerFromConfig(cfg)
		if err != nil {
			return nil, err
		}
		g.signer = s
	}

	// Default enforcer: the HTTP client to prism, unless one was injected.
	if g.enforcer == nil {
		g.enforcer = enforce.NewHTTPEnforcer(cfg.APIURL, g.signer, cfg.Timeout)
	}
	// Telemetry: cloud batcher if requested (ship to /v1/flyedge/telemetry via the signed enforcer),
	// an injected sink if one was set via WithTelemetry, else an in-memory recorder. Cloud wiring
	// happens here (post-enforcer) so the sender can reuse the enforcer's signing.
	if g.tel == nil && g.cloudTelemetry {
		if sp, ok := g.enforcer.(signedPoster); ok {
			sender := func(ctx context.Context, body []byte) error {
				_, err := sp.PostSigned(ctx, "/v1/flyedge/telemetry", body)
				return err
			}
			g.tel = telemetry.NewBatched(sender, "guard-"+randID(), g.telInterval)
		}
	}
	if g.tel == nil {
		g.tel = telemetry.NewRecorder()
	}
	return g, nil
}

// randID returns a short random id for a guard-level telemetry session.
func randID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Check runs a request through the policy decision point and returns the typed Decision. On a
// denial it ALSO returns a *DenyError (server deny/block always enforces regardless of Mode). On an
// enforcement-call failure it honors FailMode: FailOpen → allow + nil error; FailClosed → deny +
// *DenyError. A Warn decision returns (Decision{Action:Warn}, nil) — the caller decides.
func (g *Guard) Check(ctx context.Context, req CheckRequest) (Decision, error) {
	start := time.Now()
	dec, cerr := g.enforcer.Check(ctx, req)
	latencyMS := float64(time.Since(start).Microseconds()) / 1000.0

	var result Decision
	var retErr error
	ev := telemetry.Event{Stage: string(req.Stage), Model: req.Operation.ModelID, LatencyMS: latencyMS, OccurredAt: start}

	var killed *enforce.KilledError
	switch {
	case errors.As(cerr, &killed):
		// Kill switch ALWAYS enforces — never fail-open. Distinct typed error.
		result = Decision{Action: ActionDeny, Reason: "kill_switch", Message: killed.Error(), Kills: []KillInfo{killed.Kill}}
		retErr = &KillSwitchError{Kills: result.Kills}
	case cerr != nil:
		ev.Err = cerr.Error()
		if g.cfg.FailMode == FailClosed {
			result = Decision{Action: ActionDeny, Reason: "enforcement_unavailable", Message: cerr.Error()}
			retErr = &DenyError{Decision: result}
		} else {
			// fail open: allow (availability over strictness) — but the error is recorded.
			result = Decision{Action: ActionAllow, Reason: "fail_open", Message: cerr.Error()}
		}
	case len(dec.Kills) > 0:
		// Non-full-scope kill (model/tool) arrived in a 200 response — also always enforces.
		result = dec
		result.Action = ActionDeny
		if result.Reason == "" {
			result.Reason = "kill_switch"
		}
		retErr = &KillSwitchError{Kills: dec.Kills}
	case dec.Action == ActionDeny:
		result = dec
		retErr = &DenyError{Decision: dec}
	default:
		result = dec
	}

	ev.Action = string(result.Action)
	ev.Reason = result.Reason
	g.tel.Record(ev)
	return result, retErr
}

// Report returns the aggregate protection summary (checks, allowed/denied/warned/errors, timings).
// The caller decides whether/how to surface it — nothing is printed implicitly.
func (g *Guard) Report() Summary { return g.tel.Report() }

// DID returns the agent DID this guard signs as ("" if unsigned).
func (g *Guard) DID() string {
	if g.signer == nil {
		return ""
	}
	return g.signer.DID()
}

// Close flushes and releases resources (e.g. a telemetry sink's owned goroutine). Safe to call once.
func (g *Guard) Close() error {
	if g.tel != nil {
		return g.tel.Close()
	}
	return nil
}

func hasKey(cfg Config) bool { return len(cfg.KeyPEM) > 0 || cfg.KeyPEMPath != "" }

func newSignerFromConfig(cfg Config) (identity.Signer, error) {
	if len(cfg.KeyPEM) > 0 {
		return identity.NewFileSigner(cfg.KeyPEM, cfg.DID)
	}
	return identity.NewFileSignerFromPath(cfg.KeyPEMPath, cfg.DID)
}

// AsDenyError is a convenience for callers: returns the *DenyError if err is one.
func AsDenyError(err error) (*DenyError, bool) {
	var de *DenyError
	if errors.As(err, &de) {
		return de, true
	}
	return nil, false
}
