// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Package localcontrol is the in-process policy layer: detectors that decide without a network
// round trip, so an obviously-destructive call is stopped at the edge and an offline agent is not
// an ungoverned one.
//
// It is the Go port of the Python SDK's protection middleware pipeline
// (flyedge/core/middleware/protection_middlewares.py + security_middleware.py, wired in
// protection.py:_setup_middlewares). The detector set, their default patterns, and the way `mode`
// selects each detector's action are deliberately the same — an org that moves a workload from the
// Python SDK to the Go one should not silently lose a guardrail.
//
// # Local is additive, never subtractive
//
// A local verdict may add a faster NO. It must never turn the server's NO into a yes. Guard.Check
// still fires for every call: session risk accumulation, the audit trail, and every ML- or
// session-state-backed control live server-side and are untouched by anything here. That also
// means a compromised endpoint gains nothing by lying about local evaluation — the authoritative
// record is still made server-side.
//
// Consequently the only rules that belong here are unambiguous and parameter-free: a DROP TABLE, a
// pinned secret, an exhausted budget. Anything tunable, statistical, or that needs cross-session
// state stays on the server, where it can be reasoned about centrally and changed without
// redeploying every agent.
//
// # Regex portability
//
// Go's regexp is RE2: no backreferences, no lookaround. Several Python patterns use negative
// lookahead (notably "UPDATE ... SET ... not followed by WHERE"). Those are re-expressed as a
// match plus an explicit Go-side check rather than transliterated — see detectors.go. A pattern
// that silently fails to compile would be a guardrail that looks present and enforces nothing, so
// compilation errors are surfaced at construction time, never swallowed.
package localcontrol

import (
	"sort"
	"strings"

	"github.com/compfly-ai/flyedge-go/enforce"
)

// Severity ranks a finding independently of what was done about it. A CRITICAL finding in warn
// mode is still CRITICAL — the severity describes the thing found, the Action describes the
// posture applied to it.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

var severityRank = map[Severity]int{
	SeverityLow:      1,
	SeverityMedium:   2,
	SeverityHigh:     3,
	SeverityCritical: 4,
}

// AtLeast reports whether s is as severe as min. Unknown severities rank 0, so an unrecognized
// value never satisfies a threshold — an unparseable config must not silently disarm a detector.
func (s Severity) AtLeast(min Severity) bool { return severityRank[s] >= severityRank[min] }

// Request is one thing to inspect. It is deliberately flat and content-first: detectors match on
// text, and the component fields exist so scoping (which tools a detector applies to) is decidable
// without the caller pre-classifying anything.
type Request struct {
	Stage enforce.Stage
	// ComponentType is "TOOL" or "LLM", matching the wire vocabulary.
	ComponentType string
	// ComponentName is the tool or model name, used for scoping and for the audit message.
	ComponentName string
	// Content is the text to inspect: tool arguments at tool_call, model output at post_llm.
	Content string
	// SessionID scopes stateful detectors (the token budget). Empty means unscoped, which the
	// budget detector treats as a single shared bucket rather than as "no budget".
	SessionID string
	// Tokens is this call's token count, when the caller knows it. Zero means unknown, not free.
	Tokens int
}

// Verdict is one detector's finding. A nil *Verdict means "nothing to say" — detectors return nil
// on the overwhelmingly common clean path so the engine allocates nothing for it.
type Verdict struct {
	// Action is what the posture says to do about this finding.
	Action enforce.Action
	// Detector names the rule that fired, so an operator can find and tune it.
	Detector string
	// Reason is a stable machine-readable code ("destructive_query", "prompt_injection"). It is
	// part of the contract with the audit trail — rename it and dashboards break.
	Reason string
	// Message is the human sentence shown to whoever is blocked.
	Message string
	// Severity of the finding itself, independent of Action.
	Severity Severity
	// Matched is the excerpt that triggered the rule, bounded by clipMatch. Included because a
	// block with no evidence is unactionable, truncated because the content can be a whole prompt
	// and this ends up in logs.
	Matched string
}

const maxMatchExcerpt = 120

// clipMatch bounds an excerpt so a verdict cannot carry an entire prompt into the audit trail.
func clipMatch(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxMatchExcerpt {
		return s
	}
	// Byte-slicing can split a rune; trim the partial tail rather than emit invalid UTF-8.
	return strings.ToValidUTF8(s[:maxMatchExcerpt], "") + "…"
}

// Detector is one local rule. Implementations must be safe for concurrent use: one Engine is
// shared by every goroutine making calls through the Guard.
type Detector interface {
	// Name is the stable identifier used in verdicts and config.
	Name() string
	// Stages the detector runs at. A detector is never consulted at other stages.
	Stages() []enforce.Stage
	// Priority orders execution, higher first. It matters because the engine stops at the first
	// blocking verdict, so the cheapest and most certain rules should run before the fuzzier ones.
	Priority() int
	// Inspect returns a finding, or nil when there is nothing to report.
	Inspect(req *Request) *Verdict
}

// Engine runs the configured detectors for a request and reduces their findings to one verdict.
//
// It is immutable after construction: rebuilding on config change (see Apply) rather than mutating
// a live engine is what lets the hot path run lock-free while a sync goroutine swaps configuration
// underneath it.
type Engine struct {
	byStage map[enforce.Stage][]Detector
	scope   scope
	// mode is retained for reporting; the per-detector action already encodes the posture.
	mode Mode
}

// NewEngine builds an engine from an explicit detector list. Most callers want New(cfg) instead,
// which maps a synced configuration onto the detector set the way the Python SDK's
// _setup_middlewares does.
func NewEngine(mode Mode, sc scope, detectors []Detector) *Engine {
	byStage := map[enforce.Stage][]Detector{}
	for _, d := range detectors {
		for _, st := range d.Stages() {
			byStage[st] = append(byStage[st], d)
		}
	}
	for st := range byStage {
		ds := byStage[st]
		// Stable so that equal-priority detectors keep their configured order and a verdict is
		// reproducible across runs — a flapping "which rule blocked me" is very hard to debug.
		sort.SliceStable(ds, func(i, j int) bool { return ds[i].Priority() > ds[j].Priority() })
		byStage[st] = ds
	}
	return &Engine{byStage: byStage, scope: sc, mode: mode}
}

// Evaluate runs the detectors for req's stage and returns the strongest finding.
//
// Ordering is by priority and evaluation stops at the first block, so a definite DROP TABLE does
// not pay for a fuzzy injection scan. Warnings do not stop the chain: a later detector may still
// block, and reporting the block matters more than reporting the warning that preceded it.
//
// A nil Engine evaluates to allow, so "local controls not configured" needs no nil check at any
// call site.
func (e *Engine) Evaluate(req Request) Verdict {
	allow := Verdict{Action: enforce.ActionAllow}
	if e == nil {
		return allow
	}
	if !e.scope.covers(req.ComponentName, req.ComponentType) {
		return allow
	}

	var warned *Verdict
	for _, d := range e.byStage[req.Stage] {
		v := d.Inspect(&req)
		if v == nil {
			continue
		}
		switch v.Action {
		case enforce.ActionDeny:
			return *v
		case enforce.ActionWarn:
			// Keep the most severe warning rather than the first, so "the worst thing found" is
			// what surfaces when nothing blocks.
			if warned == nil || v.Severity.AtLeast(warned.Severity) {
				warned = v
			}
		}
	}
	if warned != nil {
		return *warned
	}
	return allow
}

// Detectors returns the configured detector names, for diagnostics and the sync report. Order is
// undefined; callers that display it should sort.
func (e *Engine) Detectors() []string {
	if e == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, ds := range e.byStage {
		for _, d := range ds {
			if !seen[d.Name()] {
				seen[d.Name()] = true
				out = append(out, d.Name())
			}
		}
	}
	sort.Strings(out)
	return out
}

// Mode reports the posture the engine was built with.
func (e *Engine) Mode() Mode {
	if e == nil {
		return ModeOff
	}
	return e.mode
}