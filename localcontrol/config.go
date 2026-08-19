// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package localcontrol

import (
	"fmt"
	"path"
	"strings"
)

// Mode is the local posture. It gates ONLY the local detectors in this package; a server deny or
// kill from Guard.Check enforces regardless of mode. That asymmetry is deliberate and matches the
// Python SDK: mode is how loudly the local layer speaks, not whether the platform is obeyed.
//
// The TS SDK gates cloud denies by mode; that is a bug, not a precedent worth matching.
type Mode string

const (
	// ModeOff disables local evaluation entirely.
	ModeOff Mode = "off"
	// ModeAudit records findings and never blocks — the dry run for a new rule set.
	ModeAudit Mode = "audit"
	// ModeWarn (default) surfaces findings as warnings without blocking.
	ModeWarn Mode = "warn"
	// ModeEnforce blocks on findings that meet each detector's blocking bar.
	ModeEnforce Mode = "enforce"
)

// blocks reports whether this mode permits a local block at all.
func (m Mode) blocks() bool { return m == ModeEnforce }

// normalize maps an empty or unrecognized mode onto the default. An unknown mode resolves to warn
// rather than enforce: a typo in synced config must not start blocking traffic that was previously
// allowed, and must not silently disable protection either.
func (m Mode) normalize() Mode {
	switch m {
	case ModeOff, ModeAudit, ModeWarn, ModeEnforce:
		return m
	default:
		return ModeWarn
	}
}

// Config is the synced local-control configuration — the payload the platform distributes through
// the edgesync channel, and the same shape callers can set directly for local development.
//
// It is intentionally a compiled/distilled document rather than raw policy YAML: the edge should
// not be re-deriving which controls are client-evaluable, and a smaller payload keeps the
// conditional-GET fast path cheap.
type Config struct {
	// Version is the platform's revision of this document. It is echoed in reports so an operator
	// can tell which endpoints have converged.
	Version int `json:"version"`
	// Mode is the local posture. Empty → warn.
	Mode Mode `json:"mode"`

	// Detectors toggles and tunes each built-in detector. A detector absent from the payload keeps
	// its default (enabled), because a truncated or partially-written config must fail toward
	// protection rather than away from it.
	DatabaseSafety  *DatabaseSafetyConfig  `json:"databaseSafety,omitempty"`
	SecretScan      *SecretScanConfig      `json:"secretScan,omitempty"`
	PromptInjection *PromptInjectionConfig `json:"promptInjection,omitempty"`
	TokenBudget     *TokenBudgetConfig     `json:"tokenBudget,omitempty"`

	// Scope narrows which components the local layer inspects at all. Exclusions win over
	// inclusions. Empty include lists mean "everything not excluded".
	IncludeComponents []string `json:"includeComponents,omitempty"`
	IncludePatterns   []string `json:"includePatterns,omitempty"`
	IncludeTypes      []string `json:"includeTypes,omitempty"`
	ExcludeComponents []string `json:"excludeComponents,omitempty"`
	ExcludePatterns   []string `json:"excludePatterns,omitempty"`
	ExcludeTypes      []string `json:"excludeTypes,omitempty"`
}

// DatabaseSafetyConfig tunes the destructive-query detector.
type DatabaseSafetyConfig struct {
	// Disabled turns the detector off. Expressed as an opt-OUT so that the zero value of an
	// omitted config block is "enabled", matching Config's fail-toward-protection rule.
	Disabled bool `json:"disabled,omitempty"`
	// AllowedPatterns are escape hatches: a query matching one is exempt even if it also matches a
	// danger pattern. This is how a known-safe `DELETE FROM session_cache WHERE ...` gets through.
	AllowedPatterns []string `json:"allowedPatterns,omitempty"`
	// BlockedPatterns extend the built-in danger set with org-specific rules.
	BlockedPatterns []string `json:"blockedPatterns,omitempty"`
	// ToolPatterns overrides which component names are treated as database tools. Empty keeps the
	// built-in list.
	ToolPatterns []string `json:"toolPatterns,omitempty"`
}

// SecretScanConfig tunes the secret/credential detector.
type SecretScanConfig struct {
	Disabled bool `json:"disabled,omitempty"`
	// ExtraPatterns add org-specific credential shapes (an internal token prefix, say).
	ExtraPatterns []string `json:"extraPatterns,omitempty"`
}

// PromptInjectionConfig tunes the injection detector.
type PromptInjectionConfig struct {
	Disabled bool `json:"disabled,omitempty"`
	// BlockThreshold is the minimum severity that blocks in enforce mode. Empty → high, matching
	// the Python SDK's block_threshold=ThreatLevel.HIGH.
	BlockThreshold Severity `json:"blockThreshold,omitempty"`
	// ExtraPatterns add org-specific injection phrasings.
	ExtraPatterns []string `json:"extraPatterns,omitempty"`
}

// TokenBudgetConfig caps token spend per session.
type TokenBudgetConfig struct {
	Disabled bool `json:"disabled,omitempty"`
	// MaxTokens is the per-session ceiling. Zero means no ceiling — the budget detector is inert
	// rather than blocking everything, which is what an unset limit has to mean.
	MaxTokens int `json:"maxTokens,omitempty"`
}

// scope decides which components the local layer inspects. Exclusion wins over inclusion, and an
// empty include set means "everything", mirroring ProtectionConfig.should_protect in the Python SDK.
type scope struct {
	includeComponents map[string]bool
	includePatterns   []string
	includeTypes      map[string]bool
	excludeComponents map[string]bool
	excludePatterns   []string
	excludeTypes      map[string]bool
}

func newScope(cfg Config) scope {
	return scope{
		includeComponents: toSet(cfg.IncludeComponents),
		includePatterns:   cfg.IncludePatterns,
		includeTypes:      toUpperSet(cfg.IncludeTypes),
		excludeComponents: toSet(cfg.ExcludeComponents),
		excludePatterns:   cfg.ExcludePatterns,
		excludeTypes:      toUpperSet(cfg.ExcludeTypes),
	}
}

func (s scope) covers(name, componentType string) bool {
	ct := strings.ToUpper(componentType)

	if s.excludeComponents[name] || s.excludeTypes[ct] {
		return false
	}
	for _, p := range s.excludePatterns {
		if globMatch(p, name) {
			return false
		}
	}

	if len(s.includeComponents) == 0 && len(s.includePatterns) == 0 && len(s.includeTypes) == 0 {
		return true
	}
	if s.includeComponents[name] || s.includeTypes[ct] {
		return true
	}
	for _, p := range s.includePatterns {
		if globMatch(p, name) {
			return true
		}
	}
	return false
}

// globMatch is a case-insensitive glob over a component name.
//
// path.Match treats "/" as a separator, which would make "*SQL*" fail to match a name containing
// a slash. Component names are not paths, so the separator semantics are wrong here — matching is
// done on a slash-free rendering to keep "*" meaning "any run of characters".
func globMatch(pattern, name string) bool {
	p := strings.ToLower(strings.ReplaceAll(pattern, "/", "\x00"))
	n := strings.ToLower(strings.ReplaceAll(name, "/", "\x00"))
	ok, err := path.Match(p, n)
	// A malformed pattern (unclosed "[") matches nothing rather than everything: a bad config
	// entry should not silently widen or disable a filter.
	return err == nil && ok
}

func toSet(xs []string) map[string]bool {
	if len(xs) == 0 {
		return nil
	}
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func toUpperSet(xs []string) map[string]bool {
	if len(xs) == 0 {
		return nil
	}
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[strings.ToUpper(x)] = true
	}
	return m
}

// New builds an Engine from a synced configuration.
//
// This is the Go counterpart of the Python SDK's _setup_middlewares: mode selects each detector's
// action at BUILD time rather than being re-checked per call, so the hot path is a plain loop over
// already-configured detectors.
//
// It returns an error when a configured pattern does not compile. That is deliberate: a guardrail
// that silently failed to compile would look present in the UI and enforce nothing, which is worse
// than a loud failure at config-apply time. Callers should keep the previously-good engine on error.
func New(cfg Config) (*Engine, error) {
	mode := cfg.Mode.normalize()
	if mode == ModeOff {
		return NewEngine(ModeOff, newScope(cfg), nil), nil
	}

	// In audit and warn the local layer never blocks; enforce is the only posture that can.
	block := mode.blocks()

	var detectors []Detector

	if cfg.DatabaseSafety == nil || !cfg.DatabaseSafety.Disabled {
		dbCfg := DatabaseSafetyConfig{}
		if cfg.DatabaseSafety != nil {
			dbCfg = *cfg.DatabaseSafety
		}
		d, err := newDatabaseSafety(dbCfg, block)
		if err != nil {
			return nil, fmt.Errorf("databaseSafety: %w", err)
		}
		detectors = append(detectors, d)
	}

	if cfg.SecretScan == nil || !cfg.SecretScan.Disabled {
		scCfg := SecretScanConfig{}
		if cfg.SecretScan != nil {
			scCfg = *cfg.SecretScan
		}
		d, err := newSecretScan(scCfg, block)
		if err != nil {
			return nil, fmt.Errorf("secretScan: %w", err)
		}
		detectors = append(detectors, d)
	}

	if cfg.PromptInjection == nil || !cfg.PromptInjection.Disabled {
		piCfg := PromptInjectionConfig{}
		if cfg.PromptInjection != nil {
			piCfg = *cfg.PromptInjection
		}
		d, err := newPromptInjection(piCfg, block)
		if err != nil {
			return nil, fmt.Errorf("promptInjection: %w", err)
		}
		detectors = append(detectors, d)
	}

	if cfg.TokenBudget != nil && !cfg.TokenBudget.Disabled && cfg.TokenBudget.MaxTokens > 0 {
		detectors = append(detectors, newTokenBudget(*cfg.TokenBudget, block))
	}

	return NewEngine(mode, newScope(cfg), detectors), nil
}