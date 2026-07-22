package simulation

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

// ComponentProfiler (Phase B2, observe mode) builds an agent_profile incrementally from what the
// Guard already reports: the Connect manifest (declared tools/models) plus the RuntimeEvents the
// controller records (tool invocations, LLM calls + prompt content, retriever/memory observations).
// It emits the profile as an agent_profile telemetry event so agent-eval can reason about the
// agent's attack surface and build default attack chains from it (Phase B2 attack mode).
//
// Ported from the Python flyedge/simulation/attack_injector.py ComponentProfiler, adapted to Go's
// explicit model: Go seeds tools from the manifest (it doesn't see per-call tool DEFINITIONS the way
// the Python middleware reads kwargs["tools"]), and enriches from observed calls at runtime.

// riskPatterns classify a tool by name/description into a risk level (first match wins, severity order).
var riskPatterns = []struct {
	level string
	re    *regexp.Regexp
}{
	{"critical", regexp.MustCompile(`(?i)delete|destroy|drop|purge|payment|charge|refund|transfer|password|credential|secret|token|auth|exec|shell|command|eval|run_code`)},
	{"high", regexp.MustCompile(`(?i)send|email|notify|post|webhook|publish|update|modify|write|create|insert|upload|download|approve|grant|revoke|checkout`)},
	{"medium", regexp.MustCompile(`(?i)search|query|get|read|fetch|list|lookup|find|retrieve`)},
	{"low", regexp.MustCompile(`(?i)log|cache|metric|debug|format|validate|parse|convert`)},
}

var categoryPatterns = []struct {
	category string
	re       *regexp.Regexp
}{
	{"destructive_action", regexp.MustCompile(`(?i)delete|destroy|drop|purge|remove`)},
	{"financial", regexp.MustCompile(`(?i)payment|charge|refund|transfer|invoice|billing|checkout`)},
	{"external_action", regexp.MustCompile(`(?i)send|email|notify|post|webhook|publish|sms`)},
	{"authentication", regexp.MustCompile(`(?i)login|auth|token|password|credential|sso|connect`)},
	{"code_execution", regexp.MustCompile(`(?i)exec|shell|command|eval|run_code|python|bash`)},
	{"file_access", regexp.MustCompile(`(?i)read_file|write_file|upload|download|file`)},
	{"data_mutation", regexp.MustCompile(`(?i)update|modify|write|create|insert|upsert|cart`)},
	{"data_access", regexp.MustCompile(`(?i)search|query|get|read|fetch|list|lookup|find|playlist|profile`)},
	{"internal", regexp.MustCompile(`(?i)log|cache|metric|debug|health|status|ping|whoami`)},
}

var capabilityPatterns = map[string]*regexp.Regexp{
	"has_rag":          regexp.MustCompile(`(?i)knowledge\s*base|retriev|search.*documents?|vector\s*store|RAG|embeddings?`),
	"has_memory":       regexp.MustCompile(`(?i)remember|conversation\s*history|memory|store.*preferences?|checkpoint`),
	"has_email":        regexp.MustCompile(`(?i)send.*email|notify.*customer|email.*notification|outbound.*email`),
	"has_database":     regexp.MustCompile(`(?i)database|query.*records?|customer.*data|SQL|table|collection`),
	"has_external_api": regexp.MustCompile(`(?i)external.*API|webhook|third.party|http.*request|REST.*endpoint`),
	"has_payments":     regexp.MustCompile(`(?i)payment|card|checkout|charge|billing`),
}

var guardrailPatterns = []struct {
	label string
	re    *regexp.Regexp
}{
	{"no PII sharing", regexp.MustCompile(`(?i)(?:do not|don't|never).*(?:share|reveal|expose).*(?:personal|PII|private)`)},
	{"no secret disclosure", regexp.MustCompile(`(?i)(?:never|do not|don't).*(?:token|api key|secret|password|card number)`)},
	{"single-user scope", regexp.MustCompile(`(?i)(?:only|act).*(?:authenticated|this) user|never.*another user`)},
	{"confirmation required", regexp.MustCompile(`(?i)(?:confirm|verify).*(?:before|prior|intent)`)},
}

func classifyRisk(name, desc string) string {
	text := name + " " + desc
	for _, p := range riskPatterns {
		if p.re.MatchString(text) {
			return p.level
		}
	}
	return "unknown"
}

func classifyCategory(name, desc string) string {
	text := name + " " + desc
	for _, p := range categoryPatterns {
		if p.re.MatchString(text) {
			return p.category
		}
	}
	return "general"
}

type toolInfo struct {
	name           string
	risk           string
	category       string
	discoveredFrom []string // "manifest" | "runtime"
	invocations    int
	dynamic        bool // discovered at runtime, not declared in the manifest
}

type compObs struct {
	ctype string
	name  string
	count int
}

type profiler struct {
	mu            sync.Mutex
	framework     string
	tools         map[string]*toolInfo
	obs           map[string]*compObs
	models        []string
	declaredTools []string
	llmCalls      int
	sysPrompt     map[string]any
	dirty         bool
	ready         bool
}

func newProfiler(framework string) *profiler {
	return &profiler{framework: framework, tools: map[string]*toolInfo{}, obs: map[string]*compObs{}}
}

// seedManifest pre-populates declared tools/models from the Connect manifest.
func (p *profiler) seedManifest(tools, models []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.declaredTools = append(p.declaredTools[:0], tools...)
	for _, m := range models {
		p.models = appendUnique(p.models, m)
	}
	for _, t := range tools {
		if _, ok := p.tools[t]; !ok {
			p.tools[t] = &toolInfo{name: t, risk: classifyRisk(t, ""), category: classifyCategory(t, ""), discoveredFrom: []string{"manifest"}}
		}
	}
	if len(p.tools) > 0 {
		p.ready = true
	}
	p.dirty = true
}

// observe folds one recorded RuntimeEvent into the profile.
func (p *profiler) observe(ev RuntimeEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch ev.ComponentType {
	case "llm":
		p.llmCalls++
		if ev.LLMModel != "" {
			p.models = appendUnique(p.models, ev.LLMModel)
		}
		if p.sysPrompt == nil {
			if text := llmText(ev); text != "" {
				p.parseSystemPrompt(text)
			}
		}
	case "tool":
		name := ev.ToolName
		if name == "" {
			name = ev.ComponentName
		}
		ti := p.tools[name]
		if ti == nil {
			ti = &toolInfo{name: name, risk: classifyRisk(name, ""), category: classifyCategory(name, ""), discoveredFrom: []string{"runtime"}, dynamic: true}
			p.tools[name] = ti
		} else if !containsStr(ti.discoveredFrom, "runtime") {
			ti.discoveredFrom = append(ti.discoveredFrom, "runtime")
		}
		ti.invocations++
	case "retriever":
		p.bumpObs("RETRIEVER", ev.ComponentName)
	case "checkpoint", "memory":
		p.bumpObs("CHECKPOINT", ev.ComponentName)
	}
	if !p.ready && (p.llmCalls >= 1 || len(p.tools) > 0) {
		p.ready = true
	}
	p.dirty = true
}

func (p *profiler) bumpObs(ctype, name string) {
	key := ctype + "|" + name
	if o := p.obs[key]; o != nil {
		o.count++
		return
	}
	p.obs[key] = &compObs{ctype: ctype, name: name, count: 1}
}

func (p *profiler) parseSystemPrompt(text string) {
	caps := []string{}
	for k, re := range capabilityPatterns {
		if re.MatchString(text) {
			caps = append(caps, k)
		}
	}
	sort.Strings(caps)
	guardrails := []string{}
	for _, g := range guardrailPatterns {
		if g.re.MatchString(text) {
			guardrails = append(guardrails, g.label)
		}
	}
	purpose := strings.TrimSpace(strings.SplitN(text, ".", 2)[0])
	if len(purpose) > 200 {
		purpose = purpose[:200]
	}
	p.sysPrompt = map[string]any{
		"detected":              true,
		"length":                len(text),
		"purpose":               purpose,
		"mentionedCapabilities": caps,
		"guardrails":            guardrails,
	}
}

func (p *profiler) isReady() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ready
}

func (p *profiler) isDirty() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dirty
}

// view returns the agent_profile dict WITHOUT clearing the dirty flag (used to configure the
// attack injector's default chains from the current profile).
func (p *profiler) view() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buildLocked()
}

// snapshot returns the agent_profile dict and clears the dirty flag (used when emitting telemetry).
func (p *profiler) snapshot() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dirty = false
	return p.buildLocked()
}

// buildLocked builds the agent_profile dict; the caller must hold p.mu.
func (p *profiler) buildLocked() map[string]any {
	toolsList := make([]*toolInfo, 0, len(p.tools))
	for _, t := range p.tools {
		toolsList = append(toolsList, t)
	}
	sort.Slice(toolsList, func(i, j int) bool { return toolsList[i].name < toolsList[j].name })

	critical, high := 0, 0
	componentCalls := 0
	var toolDicts []map[string]any
	hasDestructive, hasExternal, hasFinancial := false, false, false
	for _, t := range toolsList {
		if t.risk == "critical" {
			critical++
		}
		if t.risk == "high" {
			high++
		}
		componentCalls += t.invocations
		switch t.category {
		case "destructive_action":
			hasDestructive = true
		case "external_action":
			hasExternal = true
		case "financial":
			hasFinancial = true
		}
		toolDicts = append(toolDicts, map[string]any{
			"name":                t.name,
			"category":            t.category,
			"riskLevel":           t.risk,
			"invocations":         t.invocations,
			"discoveredFrom":      t.discoveredFrom,
			"dynamicRegistration": t.dynamic,
		})
	}
	for _, o := range p.obs {
		componentCalls += o.count
	}

	confidence := "low"
	if p.llmCalls >= 10 {
		confidence = "high"
	} else if p.llmCalls >= 3 || (len(p.tools) > 0 && componentCalls > 0) {
		confidence = "medium"
	}

	overall := "low"
	switch {
	case critical > 0:
		overall = "critical"
	case high > 0:
		overall = "high"
	case len(toolsList) > 0:
		overall = "medium"
	}

	profile := map[string]any{
		"confidence":    confidence,
		"lastUpdatedAt": nowUnix(),
		"discovery": map[string]any{
			"llmCallsObserved":       p.llmCalls,
			"componentCallsObserved": componentCalls,
			"profileConfidence":      confidence,
		},
		"riskSummary": map[string]any{
			"overallRisk":         overall,
			"attackSurfaces":      surfaceCount(toolsList, p.obs, p.sysPrompt, p.models),
			"criticalTools":       critical,
			"highRiskTools":       high,
			"hasDestructiveTools": hasDestructive,
			"hasExternalActions":  hasExternal,
			"hasFinancialActions": hasFinancial,
		},
	}
	if len(p.models) > 0 {
		profile["llm"] = map[string]any{"models": append([]string{}, p.models...)}
	}
	if p.sysPrompt != nil {
		profile["systemPrompt"] = p.sysPrompt
	}
	if len(toolDicts) > 0 {
		profile["tools"] = toolDicts
	}
	if len(p.declaredTools) > 0 {
		discovered := make([]string, 0, len(p.tools))
		for n := range p.tools {
			discovered = append(discovered, n)
		}
		declaredSet := toSet(p.declaredTools)
		discoveredSet := toSet(discovered)
		profile["metadataComparison"] = map[string]any{
			"declaredTools":    len(p.declaredTools),
			"discoveredTools":  len(discovered),
			"undeclaredTools":  diff(discoveredSet, declaredSet),
			"declaredButNotSeen": diff(declaredSet, discoveredSet),
		}
	}
	return profile
}

func surfaceCount(tools []*toolInfo, obs map[string]*compObs, sys map[string]any, models []string) int {
	n := 0
	if len(tools) > 0 {
		n++
	}
	if len(obs) > 0 || (sys != nil) {
		n++
	}
	if len(models) > 0 {
		n++
	}
	return n
}

// --- small helpers ---

func llmText(ev RuntimeEvent) string {
	if ev.LLMResponse != "" {
		return ev.LLMResponse
	}
	var parts []string
	for _, m := range ev.LLMMessages {
		if c, ok := m["content"].(string); ok {
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, "\n")
}

func appendUnique(xs []string, v string) []string {
	if containsStr(xs, v) {
		return xs
	}
	return append(xs, v)
}

func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func diff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
