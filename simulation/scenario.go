// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CompFly AI
package simulation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
)

const maxScenarioEvents = 1024
const maxScenarioBytes = 4 * 1024 * 1024

type scenarioPlan struct {
	ID          string         `json:"id"`
	Session     string         `json:"session_id"`
	Step        string         `json:"step_id"`
	Overrides   []scenarioRule `json:"overrides"`
	OnUnmatched string         `json:"on_unmatched"`
}
type scenarioRule struct {
	ID         string          `json:"id"`
	Enabled    *bool           `json:"enabled,omitempty"`
	Tool       string          `json:"tool"`
	Occurrence int             `json:"occurrence"`
	Arguments  map[string]any  `json:"arguments"`
	Value      json.RawMessage `json:"value"`
	Required   *int            `json:"required_matches"`
}

func (r scenarioRule) enabled() bool { return r.Enabled == nil || *r.Enabled }

type scenarioMocks struct {
	raw         any
	plan        *scenarioPlan
	err         string
	occurrences map[string]int
	matches     map[string]int
	events      []map[string]any
	bytes       int
}

func scenarioRaw(extra json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if len(extra) == 0 {
		return nil
	}
	if json.Unmarshal(extra, &fields) != nil {
		return json.RawMessage(`{"invalid":true}`)
	}
	return fields["scenario_overrides"]
}
func (m *scenarioMocks) configure(raw json.RawMessage) {
	var normalized any
	if len(raw) > 0 && json.Unmarshal(raw, &normalized) != nil {
		normalized = string(raw)
	}
	if reflect.DeepEqual(m.raw, normalized) {
		return
	}
	*m = scenarioMocks{raw: normalized, occurrences: map[string]int{}, matches: map[string]int{}, events: []map[string]any{}}
	if normalized == nil {
		return
	}
	if len(raw) > 256*1024 {
		m.err = "scenario mock plan exceeds 256 KiB"
		return
	}
	var p scenarioPlan
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		m.err = "invalid scenario mock plan: " + err.Error()
		return
	}
	if d.Decode(new(any)) != io.EOF {
		m.err = "invalid scenario mock plan"
		return
	}
	if p.ID == "" || p.Session == "" || p.Step == "" || p.Overrides == nil || len(p.Overrides) > 64 || (p.OnUnmatched != "" && p.OnUnmatched != "fail" && p.OnUnmatched != "passthrough") {
		m.err = "invalid scenario mock scope"
		return
	}
	ids := map[string]bool{}
	for _, r := range p.Overrides {
		if r.ID == "" || ids[r.ID] || r.Tool == "" || r.Occurrence < 1 || r.Required == nil || *r.Required < 0 || *r.Required > 1 || r.Arguments == nil || len(r.Value) == 0 {
			m.err = "invalid scenario mock rule"
			return
		}
		ids[r.ID] = true
		for path := range r.Arguments {
			if path != "" && !strings.HasPrefix(path, "/") {
				m.err = "invalid scenario mock pointer"
				return
			}
		}
	}
	m.plan = &p
}
func (m *scenarioMocks) append(event map[string]any) {
	b, err := json.Marshal(event)
	if err != nil || len(m.events) >= maxScenarioEvents || m.bytes+len(b) > maxScenarioBytes {
		m.err = "scenario evidence limit exceeded"
		return
	}
	// Own the evidence; callers may mutate their arguments after returning.
	var owned map[string]any
	_ = json.Unmarshal(b, &owned)
	m.events = append(m.events, owned)
	m.bytes += len(b)
}
func (m *scenarioMocks) observe(session, event, name string, detail map[string]any) {
	p := m.plan
	if p == nil || session != p.Session {
		return
	}
	detail["event"] = event
	detail["name"] = name
	detail["component"] = "tool"
	detail["plan_id"] = p.ID
	detail["step_id"] = p.Step
	detail["session_id"] = session
	m.append(detail)
}
func (m *scenarioMocks) before(session, tool string, args any, allowed bool) (json.RawMessage, bool, error) {
	fail := func(message string) (json.RawMessage, bool, error) {
		return nil, false, fmt.Errorf("scenario mock failure: %s", message)
	}
	if m.err != "" {
		return fail(m.err)
	}
	p := m.plan
	if p == nil {
		return nil, false, nil
	}
	if session != p.Session {
		return fail("scenario mock session mismatch")
	}
	enabled := false
	for _, r := range p.Overrides {
		enabled = enabled || r.enabled()
	}
	if enabled && !allowed {
		return fail("scenario mocking not authorized")
	}
	m.occurrences[tool]++
	var match *scenarioRule
	isolated := false
	for i := range p.Overrides {
		r := &p.Overrides[i]
		if !r.enabled() || r.Tool != tool {
			continue
		}
		isolated = true
		if r.Occurrence != m.occurrences[tool] {
			continue
		}
		matches := true
		for path, want := range r.Arguments {
			got, ok := jsonPointer(args, path)
			if !ok || !reflect.DeepEqual(got, want) {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		if match != nil {
			m.err = "scenario mock conflict"
			return fail(m.err)
		}
		match = r
	}
	if match == nil {
		if isolated && p.OnUnmatched != "passthrough" {
			m.err = fmt.Sprintf("no scenario mock matched tool %s occurrence %d", tool, m.occurrences[tool])
			m.observe(session, "component.mock_rejected", tool, map[string]any{"occurrence": m.occurrences[tool], "reason": m.err})
			return fail(m.err)
		}
		return nil, false, nil
	}
	m.matches[match.ID]++
	var value any
	_ = json.Unmarshal(match.Value, &value)
	m.observe(session, "component.mocked", tool, map[string]any{"rule_id": match.ID, "arguments": args, "result": value})
	if m.err != "" {
		return fail(m.err)
	}
	return append(json.RawMessage(nil), match.Value...), true, nil
}
func jsonPointer(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	for _, part := range strings.Split(path[1:], "/") {
		// Reject invalid RFC 6901 escapes instead of matching a different key.
		for i := 0; i < len(part); i++ {
			if part[i] == '~' {
				if i+1 == len(part) || (part[i+1] != '0' && part[i+1] != '1') {
					return nil, false
				}
				i++
			}
		}
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			v, ok = x[part]
			if !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(x) || strconv.Itoa(i) != part {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}
func (m *scenarioMocks) status() map[string]any {
	var err any
	if m.err != "" {
		err = m.err
	}
	if m.plan == nil {
		return map[string]any{"plan_id": nil, "error": err, "complete": m.err == ""}
	}
	complete := m.err == ""
	for _, r := range m.plan.Overrides {
		if r.enabled() && m.matches[r.ID] != *r.Required {
			complete = false
		}
	}
	return map[string]any{"plan_id": m.plan.ID, "session_id": m.plan.Session, "step_id": m.plan.Step, "error": err, "matches": m.matches, "events": m.events, "complete": complete}
}

// ScenarioStatus returns an owned copy of installed-plan evidence.
func (c *Controller) ScenarioStatus() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.mocks.status()
	s["run_id"] = c.runID
	b, _ := json.Marshal(s)
	var owned map[string]any
	_ = json.Unmarshal(b, &owned)
	return owned
}

// BeforeScenarioTool is called after policy approval and before real execution.
func (c *Controller) BeforeScenarioTool(session, tool string, args any, allowed bool) (json.RawMessage, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != StateActive {
		return nil, false, nil
	}
	if c.protDisabled {
		return nil, false, fmt.Errorf("scenario mocks cannot disable protection")
	}
	return c.mocks.before(session, tool, args, allowed)
}
func (c *Controller) ObserveScenarioTool(session, event, tool string, detail map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == StateActive {
		c.mocks.observe(session, event, tool, detail)
	}
}

// ScenarioToken identifies the exact plan content captured at dispatch.
func (c *Controller) ScenarioToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scenarioTokenLocked()
}
func (c *Controller) scenarioTokenLocked() string {
	if c.mocks.plan == nil {
		return ""
	}
	raw, _ := json.Marshal(c.mocks.raw)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

// FinishScenario only completes the exact plan captured before handler dispatch.
func (c *Controller) FinishScenario(run, session, plan string, success bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.mocks.plan
	if c.state == StateActive && p != nil && run == c.runID && session == p.Session && plan == c.scenarioTokenLocked() {
		c.scenarioSnapshotLocked("complete", success)
	}
}
func (c *Controller) scenarioSnapshotLocked(phase string, success bool) {
	p := c.mocks.plan
	if p == nil || c.tr == nil {
		return
	}
	correlation := RuntimeEvent{PromptID: p.Session}
	correlateEvent(&correlation)
	event := map[string]any{"event_type": "scenario.snapshot", "component_type": "agent", "component_name": "scenario", "run_id": c.runID, "timestamp": nowUnix(), "session_id": correlation.SessionID, "prompt_id": correlation.PromptID, "scenario_snapshot": c.mocks.status(), "scenario_phase": phase, "scenario_success": success}
	b, err := json.Marshal(event)
	if err != nil || !c.tr.trySend(string(b)) {
		c.mocks.err = "scenario snapshot delivery unavailable"
	}
}
