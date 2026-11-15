// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CompFly AI

package flyedge

import (
	"context"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const maxInvocationUsageCounter = uint64(1<<53 - 1)

// ModelUsage contains provider-measured counts for one started model attempt.
// Nil means unknown; a pointer to zero means measured zero. InputTokens includes
// cached input, unlike LLMCall's legacy uncached scalar. Do not supply an estimated
// total. Failed or interrupted attempts must leave output/reasoning/total unknown.
type ModelUsage struct {
	InputTokens       *uint64 `json:"input_tokens"`
	CachedInputTokens *uint64 `json:"cached_input_tokens"`
	OutputTokens      *uint64 `json:"output_tokens"`
	ReasoningTokens   *uint64 `json:"reasoning_tokens"`
	TotalTokens       *uint64 `json:"total_tokens"`
}

type invocationUsageRow struct {
	ModelUsage
	ModelCalls    uint32 `json:"model_calls"`
	ReportedCalls uint32 `json:"reported_calls"`
	UnknownCalls  uint32 `json:"unknown_calls"`
}

type invocationModelUsage struct {
	invocationUsageRow
	Model string `json:"model"`
}

type invocationUsageEnvelope struct {
	invocationUsageRow
	Version      uint8                  `json:"version"`
	InvocationID string                 `json:"invocation_id"`
	SessionID    string                 `json:"session_id"`
	ByModel      []invocationModelUsage `json:"by_model"`
}

type invocationUsageContextKey struct{}
type invocationUsage struct {
	mu                  sync.Mutex
	guard               *Guard
	invocation, session string
	closed, invalid     bool
	seen                map[string]bool
	pending             map[string]string
	models              map[string]*invocationUsageRow
	total               invocationUsageRow
}

func newInvocationUsage(g *Guard, invocation, session string) *invocationUsage {
	return &invocationUsage{guard: g, invocation: invocation, session: session,
		seen: make(map[string]bool), pending: make(map[string]string), models: make(map[string]*invocationUsageRow)}
}

// RecordInvocationUsage records one model attempt within this Guard's admitted
// invocation context. It does not emit another telemetry event. Reusing callID
// deduplicates observations; separate attempts need separate IDs. Nil usage records
// an unknown attempt, so a later measured call cannot hide it. Other guards,
// unbound contexts and observations after completion are ignored. Invalid/bounded
// observations suppress the envelope rather than exposing an incomplete total.
func (g *Guard) RecordInvocationUsage(ctx context.Context, callID, model string, usage *ModelUsage) {
	if g == nil || ctx == nil {
		return
	}
	scope, _ := ctx.Value(invocationUsageContextKey{}).(*invocationUsage)
	if scope == nil || scope.guard != g {
		return
	}
	counts := cloneModelUsage(usage)
	if ctx.Err() != nil {
		counts.OutputTokens, counts.ReasoningTokens, counts.TotalTokens = nil, nil, nil
	}
	scope.record(callID, model, counts)
}

func usageCount(value uint64) *uint64 { return &value }

func cloneModelUsage(usage *ModelUsage) ModelUsage {
	if usage == nil {
		return ModelUsage{}
	}
	result := *usage
	for _, field := range []**uint64{&result.InputTokens, &result.CachedInputTokens, &result.OutputTokens, &result.ReasoningTokens, &result.TotalTokens} {
		if *field != nil {
			*field = usageCount(**field)
		}
	}
	return result
}

func (u *ModelUsage) valid() bool {
	for _, count := range []*uint64{u.InputTokens, u.CachedInputTokens, u.OutputTokens, u.ReasoningTokens, u.TotalTokens} {
		if count != nil && *count > maxInvocationUsageCounter {
			return false
		}
	}
	if u.InputTokens != nil && u.OutputTokens != nil {
		if *u.OutputTokens > maxInvocationUsageCounter-*u.InputTokens {
			return false
		}
		known := *u.InputTokens + *u.OutputTokens
		if u.TotalTokens == nil {
			u.TotalTokens = usageCount(known)
		}
		if *u.TotalTokens < known {
			return false
		}
	} else if u.TotalTokens != nil {
		if (u.InputTokens != nil && *u.InputTokens > *u.TotalTokens) || (u.OutputTokens != nil && *u.OutputTokens > *u.TotalTokens) {
			return false
		}
	}
	return !(u.InputTokens != nil && u.CachedInputTokens != nil && *u.CachedInputTokens > *u.InputTokens) &&
		!(u.OutputTokens != nil && u.ReasoningTokens != nil && *u.ReasoningTokens > *u.OutputTokens)
}

func addUsageCount(total **uint64, count *uint64) bool {
	if count == nil {
		return true
	}
	current := uint64(0)
	if *total != nil {
		current = **total
	}
	if *count > maxInvocationUsageCounter-current {
		return false
	}
	*total = usageCount(current + *count)
	return true
}

func (row *invocationUsageRow) add(counts ModelUsage) bool {
	row.ModelCalls++
	if counts.TotalTokens != nil {
		row.ReportedCalls++
	}
	row.UnknownCalls = row.ModelCalls - row.ReportedCalls
	return addUsageCount(&row.InputTokens, counts.InputTokens) &&
		addUsageCount(&row.CachedInputTokens, counts.CachedInputTokens) &&
		addUsageCount(&row.OutputTokens, counts.OutputTokens) &&
		addUsageCount(&row.ReasoningTokens, counts.ReasoningTokens) &&
		addUsageCount(&row.TotalTokens, counts.TotalTokens)
}

func (row invocationUsageRow) finalized() (invocationUsageRow, bool) {
	if row.UnknownCalls > 0 {
		row.TotalTokens = nil
	}
	// Independently partial dimensions can become inconsistent when combined.
	// Validate the final row too, without manufacturing a partial row's total.
	counts := cloneModelUsage(&row.ModelUsage)
	return row, counts.valid()
}

func (u *invocationUsage) record(callID, model string, counts ModelUsage) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, pending := u.pending[callID]; pending {
		return
	}
	u.recordLocked(callID, model, counts)
}

func validUsageIdentity(callID, model string) bool {
	return callID != "" && len(callID) <= 128 && utf8.ValidString(callID) && !strings.ContainsFunc(callID, unicode.IsControl) &&
		len(model) <= 256 && utf8.ValidString(model) && strings.TrimSpace(model) == model && !strings.ContainsFunc(model, unicode.IsControl)
}

// Reserve a started transport attempt before handing control to the provider.
// An unread/unclosed response must remain unknown when its handler completes.
func (u *invocationUsage) start(callID, model string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed || u.invalid || u.seen[callID] {
		return false
	}
	if _, exists := u.pending[callID]; exists {
		return false
	}
	if model == "" {
		model = "unknown"
	}
	if !validUsageIdentity(callID, model) || len(u.seen)+len(u.pending) >= 256 || (u.models[model] == nil && len(u.models) >= 64) {
		u.invalid = true
		return false
	}
	if u.models[model] == nil {
		u.models[model] = &invocationUsageRow{}
	}
	u.pending[callID] = model
	return true
}

func (u *invocationUsage) complete(ctx context.Context, callID string, usage *ModelUsage) {
	counts := cloneModelUsage(usage)
	if ctx.Err() != nil {
		counts.OutputTokens, counts.ReasoningTokens, counts.TotalTokens = nil, nil, nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	model, pending := u.pending[callID]
	if !pending || u.closed {
		return
	}
	delete(u.pending, callID)
	u.recordLocked(callID, model, counts)
}

func (u *invocationUsage) recordLocked(callID, model string, counts ModelUsage) {
	if u.closed || u.invalid || u.seen[callID] {
		return
	}
	if model == "" {
		model = "unknown"
	}
	if !validUsageIdentity(callID, model) || len(u.seen)+len(u.pending) >= 256 || (u.models[model] == nil && len(u.models) >= 64) || !counts.valid() {
		u.invalid = true
		return
	}
	u.seen[callID] = true
	row := u.models[model]
	if row == nil {
		row = &invocationUsageRow{}
		u.models[model] = row
	}
	if !row.add(counts) || !u.total.add(counts) {
		u.invalid = true
	}
}

func (u *invocationUsage) abandon() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.closed = true
	u.models, u.seen = nil, nil
	u.pending = nil
}

func (u *invocationUsage) finish() *invocationUsageEnvelope {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return nil
	}
	for id, model := range u.pending {
		delete(u.pending, id)
		u.recordLocked(id, model, ModelUsage{})
	}
	u.closed = true
	defer func() { u.models, u.seen, u.pending = nil, nil, nil }()
	if u.invalid || len(u.seen) == 0 {
		return nil
	}
	total, valid := u.total.finalized()
	if !valid {
		return nil
	}
	result := &invocationUsageEnvelope{invocationUsageRow: total, Version: 1, InvocationID: u.invocation, SessionID: u.session}
	models := make([]string, 0, len(u.models))
	for model := range u.models {
		models = append(models, model)
	}
	sort.Strings(models)
	for _, model := range models {
		row, valid := u.models[model].finalized()
		if !valid {
			return nil
		}
		result.ByModel = append(result.ByModel, invocationModelUsage{invocationUsageRow: row, Model: model})
	}
	return result
}
