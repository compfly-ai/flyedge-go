// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"sync/atomic"
	"time"

	"github.com/compfly-ai/flyedge-go/enforce"
	"github.com/compfly-ai/flyedge-go/localcontrol"
	"github.com/compfly-ai/flyedge-go/telemetry"
)

// MetadataTokens is the CheckRequest.Metadata key the token-budget detector reads.
//
// It lives in metadata rather than as a CheckRequest field because the request JSON is the frozen
// wire schema shared with prism and the other SDKs (DESIGN.md §1b); adding a field to feed a
// purely local detector would be a wire change for no server-side reader.
const MetadataTokens = "tokens"

// tokensFromMetadata reads this call's token count, if the caller supplied one. Unknown reads as
// zero, which the budget detector treats as "no spend to charge" rather than as free.
func tokensFromMetadata(md map[string]any) int {
	switch v := md[MetadataTokens].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64: // survives a JSON round trip
		return int(v)
	default:
		return 0
	}
}

// Local controls: the in-process policy layer, and how it composes with the server's decision.
//
// The rule, stated once: local evaluation may add a faster NO. It may never turn the server's NO
// into a yes, and it never suppresses a server call that would have happened anyway on an allowed
// path. See the localcontrol package doc for why only unambiguous rules belong locally.

// localEngine holds the active engine. atomic.Pointer rather than a mutex because Check reads it
// on every call and the sync goroutine writes it roughly never — a swap must not make the hot path
// pay for a lock.
type localEngineHolder struct {
	ptr atomic.Pointer[localcontrol.Engine]
}

func (h *localEngineHolder) load() *localcontrol.Engine { return h.ptr.Load() }

func (h *localEngineHolder) store(e *localcontrol.Engine) { h.ptr.Store(e) }

// WithLocalControls enables in-process evaluation with an explicit configuration.
//
// Most deployments should let the platform supply this through the sync channel
// (SyncLocalControls) rather than hardcoding it, so a rule change does not require redeploying
// every agent. This option exists for local development and for tests.
//
// It returns an error when a configured pattern does not compile, so a broken rule set fails at
// New rather than looking active and enforcing nothing.
func WithLocalControls(cfg localcontrol.Config) Option {
	return func(g *Guard) error {
		e, err := localcontrol.New(cfg)
		if err != nil {
			return err
		}
		g.local.store(e)
		return nil
	}
}

// WithLocalControlEngine injects an already-built engine. Useful when a caller wants to share one
// engine across several Guards, or to inject a stub in tests.
func WithLocalControlEngine(e *localcontrol.Engine) Option {
	return func(g *Guard) error { g.local.store(e); return nil }
}

// SetLocalControls swaps the active local-control configuration at runtime. This is what the sync
// channel calls when the platform publishes a new rule set.
//
// On a bad configuration the previous engine is KEPT and the error returned: a rule set that fails
// to compile must not disarm the protection that was already running. Callers should log and
// retry on the next sync rather than treating it as fatal.
func (g *Guard) SetLocalControls(cfg localcontrol.Config) error {
	e, err := localcontrol.New(cfg)
	if err != nil {
		return err
	}
	g.local.store(e)
	return nil
}

// LocalControlDetectors reports the currently active local detectors, for diagnostics and for the
// sync channel's status report. Nil when local controls are not configured.
func (g *Guard) LocalControlDetectors() []string { return g.local.load().Detectors() }

// evaluateLocal runs the local layer for a request.
//
// Returns a decision and true when the local layer blocks — the caller should short-circuit. The
// returned warnings are carried into whatever the server decides, so a local finding that did not
// block is still reported rather than dropped.
func (g *Guard) evaluateLocal(req CheckRequest) (Decision, []string, bool) {
	e := g.local.load()
	if e == nil {
		return Decision{}, nil, false
	}

	v := e.Evaluate(localcontrol.Request{
		Stage:         req.Stage,
		ComponentType: req.ComponentType,
		ComponentName: req.ComponentName,
		Content:       req.Content.Full,
		SessionID:     req.SessionID,
		Tokens:        tokensFromMetadata(req.Metadata),
	})

	switch v.Action {
	case enforce.ActionDeny:
		return Decision{
			Action:  enforce.ActionDeny,
			Reason:  v.Reason,
			Message: v.Message,
			// The detector and the evidence ride along so the block is actionable without
			// correlating against a server-side record that, for a locally-blocked call, may be
			// the only place the detail would otherwise exist.
			Warnings: []string{"local:" + v.Detector, "matched:" + v.Matched},
		}, nil, true
	case enforce.ActionWarn:
		return Decision{}, []string{"local:" + v.Detector + ":" + v.Reason}, false
	default:
		return Decision{}, nil, false
	}
}

// recordLocalBlock emits telemetry for a locally-blocked call.
//
// A local block skips the /check round trip — that is the latency win and what makes the layer
// work offline — so this is the only record that the call was stopped. Emitting it is what keeps
// "local enforcement" from meaning "invisible enforcement".
func (g *Guard) recordLocalBlock(req CheckRequest, dec Decision, tr traceIDs) {
	g.tel.Record(telemetry.Event{
		Stage:        string(req.Stage),
		Model:        req.Operation.ModelID,
		Action:       string(dec.Action),
		Reason:       dec.Reason,
		OccurredAt:   time.Now(),
		SessionID:    req.SessionID,
		RequestID:    req.RequestID,
		TraceID:      tr.traceID,
		SpanID:       tr.spanID,
		ParentSpanID: tr.parentSpan,
	})
}

// traceIDs groups the correlation ids Check derives, so the local path and the server path record
// the same span identity without threading three strings through every signature.
type traceIDs struct {
	traceID    string
	spanID     string
	parentSpan string
}