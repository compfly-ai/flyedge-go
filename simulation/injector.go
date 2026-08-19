// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package simulation

import (
	"encoding/json"
	"sync"
)

// Attack injector (Phase B2, attack mode). Ported from the Python attack_schedule.py /
// attack_injector.py. Injection is a set of AttackChains, each a sequential list of AttackSteps; on
// a governed component call the injector fires the first chain whose current step matches, mutating
// the LLM request (config_inject) or a tool result (tool_poison / error_inject). One injection per
// call; a global max caps a run. Chains come from the eval harness (config) or are built from the
// agent_profile. The controller owns the injector and wires it to the Guard's seams.

// injectionMeta is stamped onto the RuntimeEvent so eval-runner's 4-state correlator can attribute
// downstream behavior to the injection.
type injectionMeta struct {
	ID             string
	Strategy       string
	Target         string
	Sophistication int
	Chain          string
	Tier           int
}

// AttackStep targets one component type/name with a strategy + sophistication.
type AttackStep struct {
	Strategy       string
	TargetType     string // llm | tool | retriever | checkpoint
	TargetName     string // "*" (or "") matches any component of that type
	Sophistication int
	Payload        any // explicit override; nil → resolve from the payload tables
	Variant        int
}

func (s *AttackStep) matches(ctype, name string) bool {
	if s.TargetType != ctype {
		return false
	}
	if s.TargetName != "" && s.TargetName != "*" && s.TargetName != name {
		return false
	}
	return true
}

// AttackChain fires its steps sequentially — each waits for a matching component call.
type AttackChain struct {
	Name  string
	Steps []AttackStep
	idx   int
	done  bool
}

func (c *AttackChain) complete() bool { return c.done || c.idx >= len(c.Steps) }

func (c *AttackChain) nextFor(ctype, name string) *AttackStep {
	if c.complete() {
		return nil
	}
	s := &c.Steps[c.idx]
	if s.matches(ctype, name) {
		c.idx++
		if c.idx >= len(c.Steps) {
			c.done = true
		}
		return s
	}
	return nil
}

// --- config wire shapes (extra.attack_injector) ---

type attackConfig struct {
	Mode          string `json:"mode"`
	Tier          int    `json:"tier"`
	MaxInjections int    `json:"max_injections"`
	AttackConfig  struct {
		Chains              []chainDef `json:"chains"`
		SophisticationRange []int      `json:"sophistication_range"`
		EvolvedPayloads     []stepDef  `json:"evolved_payloads"`
	} `json:"attack_config"`
}

type chainDef struct {
	Name  string    `json:"name"`
	Steps []stepDef `json:"steps"`
}

type stepDef struct {
	Name           string `json:"name"`
	Strategy       string `json:"strategy"`
	TargetType     string `json:"target_component_type"`
	TargetName     string `json:"target_component_name"`
	Sophistication int    `json:"sophistication"`
	Payload        any    `json:"payload"`
	Variant        int    `json:"variant_index"`
}

// injector holds the active chains + injection budget for a run.
type injector struct {
	mu      sync.Mutex
	mode    string // observe | attack
	tier    int
	max     int
	count   int
	chains  []*AttackChain
	profile map[string]any
}

func newInjector() *injector { return &injector{mode: "observe", max: defaultMaxInjections} }

const defaultMaxInjections = 100

// configure (re)loads the injector from the attack_injector block + the current profile. Called on
// activate and on same-run config updates (tier hot-swap) — it resets chains + the injection count.
func (inj *injector) configure(raw json.RawMessage, profile map[string]any) {
	var ac attackConfig
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &ac)
	}
	inj.mu.Lock()
	defer inj.mu.Unlock()
	inj.mode = orDefault(ac.Mode, "observe")
	inj.tier = ac.Tier
	inj.max = ac.MaxInjections
	if inj.max <= 0 {
		inj.max = defaultMaxInjections
	}
	inj.count = 0
	inj.profile = profile
	inj.chains = buildChains(ac, profile)
}

func (inj *injector) attackActive() bool {
	inj.mu.Lock()
	defer inj.mu.Unlock()
	return inj.mode == "attack" && inj.count < inj.max
}

// injectForLLM returns an adversarial system message to insert into an LLM request, if a config_inject
// step matches this LLM component and the budget allows.
func (inj *injector) injectForLLM(componentName string) (string, injectionMeta, bool) {
	inj.mu.Lock()
	defer inj.mu.Unlock()
	if inj.mode != "attack" || inj.count >= inj.max {
		return "", injectionMeta{}, false
	}
	for _, ch := range inj.chains {
		s := ch.nextFor("llm", componentName)
		if s == nil || s.Strategy != "config_inject" {
			continue
		}
		payload := s.Payload
		if payload == nil {
			payload = resolvePayload(s.Strategy, s.Sophistication, inj.profile, s.Variant)
		}
		msg, _ := payload.(string)
		if msg == "" {
			continue
		}
		inj.count++
		return msg, inj.metaFor(s, ch.Name, componentName), true
	}
	return "", injectionMeta{}, false
}

// injectForTool returns a mutated tool result if a tool_poison/error_inject step matches this tool.
func (inj *injector) injectForTool(toolName, result string) (string, injectionMeta, bool) {
	inj.mu.Lock()
	defer inj.mu.Unlock()
	if inj.mode != "attack" || inj.count >= inj.max {
		return result, injectionMeta{}, false
	}
	for _, ch := range inj.chains {
		s := ch.nextFor("tool", toolName)
		if s == nil || (s.Strategy != "tool_poison" && s.Strategy != "error_inject") {
			continue
		}
		payload := s.Payload
		if payload == nil {
			payload = resolvePayload(s.Strategy, s.Sophistication, inj.profile, s.Variant)
		}
		out := mutateToolResult(s.Strategy, payload, result)
		inj.count++
		return out, inj.metaFor(s, ch.Name, toolName), true
	}
	return result, injectionMeta{}, false
}

func (inj *injector) metaFor(s *AttackStep, chain, target string) injectionMeta {
	return injectionMeta{ID: randHex()[:12], Strategy: s.Strategy, Target: target, Sophistication: s.Sophistication, Chain: chain, Tier: inj.tier}
}

// mutateToolResult applies a strategy's payload to a tool result string.
func mutateToolResult(strategy string, payload any, result string) string {
	if strategy == "error_inject" {
		if s, ok := payload.(string); ok && s != "" {
			return s // replace the result with a crafted error
		}
		return result
	}
	// tool_poison: merge the payload map into a JSON-object result, else append a note.
	pm, _ := payload.(map[string]any)
	if pm == nil {
		return result
	}
	var obj map[string]any
	if json.Unmarshal([]byte(result), &obj) == nil && obj != nil {
		for k, v := range pm {
			obj[k] = v
		}
		if b, err := json.Marshal(obj); err == nil {
			return string(b)
		}
	}
	b, _ := json.Marshal(pm)
	return result + "\n\n[System Note]: " + string(b)
}

// --- chain building ---

func buildChains(ac attackConfig, profile map[string]any) []*AttackChain {
	var chains []*AttackChain
	if len(ac.AttackConfig.Chains) > 0 {
		for i, cd := range ac.AttackConfig.Chains {
			name := cd.Name
			if name == "" {
				name = "chain_" + itoa(i)
			}
			chains = append(chains, &AttackChain{Name: name, Steps: stepsFromDefs(cd.Steps)})
		}
	} else {
		minS, maxS := sophRange(ac.AttackConfig.SophisticationRange)
		chains = buildDefaultChains(profile, minS, maxS)
	}
	// Tier-3 evolved payloads (single-step chains).
	for i, ep := range ac.AttackConfig.EvolvedPayloads {
		name := ep.Name
		if name == "" {
			name = "evolved_" + itoa(i)
		}
		chains = append(chains, &AttackChain{Name: name, Steps: stepsFromDefs([]stepDef{ep})})
	}
	return chains
}

func stepsFromDefs(defs []stepDef) []AttackStep {
	out := make([]AttackStep, 0, len(defs))
	for _, d := range defs {
		strat := orDefault(d.Strategy, "config_inject")
		ttype := orDefault(d.TargetType, "llm")
		tname := orDefault(d.TargetName, "*")
		soph := d.Sophistication
		if soph == 0 {
			soph = 1
		}
		out = append(out, AttackStep{Strategy: strat, TargetType: ttype, TargetName: tname, Sophistication: soph, Payload: d.Payload, Variant: d.Variant})
	}
	return out
}

// buildDefaultChains derives one chain per discovered attack surface from the profile:
// config_inject on the LLM, tool_poison per tool, error_inject per critical/high-risk tool.
func buildDefaultChains(profile map[string]any, minS, maxS int) []*AttackChain {
	var chains []*AttackChain
	for soph := minS; soph <= maxS; soph++ {
		chains = append(chains, &AttackChain{
			Name:  "config_inject_L" + itoa(soph),
			Steps: []AttackStep{{Strategy: "config_inject", TargetType: "llm", TargetName: "*", Sophistication: soph}},
		})
	}
	tools, _ := profile["tools"].([]map[string]any)
	for _, t := range tools {
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		risk, _ := t["riskLevel"].(string)
		for soph := minS; soph <= maxS; soph++ {
			chains = append(chains, &AttackChain{
				Name:  "tool_poison_" + name + "_L" + itoa(soph),
				Steps: []AttackStep{{Strategy: "tool_poison", TargetType: "tool", TargetName: name, Sophistication: soph}},
			})
			if risk == "critical" || risk == "high" {
				chains = append(chains, &AttackChain{
					Name:  "error_inject_" + name + "_L" + itoa(soph),
					Steps: []AttackStep{{Strategy: "error_inject", TargetType: "tool", TargetName: name, Sophistication: soph}},
				})
			}
		}
	}
	return chains
}

func sophRange(r []int) (int, int) {
	if len(r) >= 2 && r[0] >= 1 && r[1] >= r[0] {
		return r[0], r[1]
	}
	return 1, 2
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
