package localcontrol

import (
	"fmt"
	"regexp"
	"sync"

	"github.com/compfly-ai/flyedge-go/enforce"
)

// action resolves a detector's posture: block when the mode allows blocking and the finding meets
// the detector's bar, otherwise warn. Every detector funnels through this so "audit and warn never
// block" is one decision, not four.
func action(block bool) enforce.Action {
	if block {
		return enforce.ActionDeny
	}
	return enforce.ActionWarn
}

func compileAll(patterns []string, flags string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		re, err := regexp.Compile(flags + p)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

// ── Database safety ───────────────────────────────────────────────────────────
//
// Port of DatabaseSafetyMiddleware. Blocks the operations that are irreversible and almost never
// intended from an agent: DROP, TRUNCATE, unqualified DELETE/UPDATE, privilege grants.

// dangerPattern pairs a compiled rule with what to call it and how bad it is.
type dangerPattern struct {
	re       *regexp.Regexp
	op       string
	severity Severity
	// requiresNoWhere marks patterns whose Python original used negative lookahead to mean "and
	// there is no WHERE clause". RE2 has no lookaround, so the check is done in Go instead of
	// being transliterated into a regex that would quietly match the wrong thing.
	requiresNoWhere bool
}

// The built-in danger set, kept in the same order and with the same severities as the Python
// SDK's DANGER_PATTERNS so that a query blocked by one SDK is blocked by the other.
var builtinDangerPatterns = []struct {
	pattern         string
	op              string
	severity        Severity
	requiresNoWhere bool
}{
	{`\bDROP\s+(TABLE|DATABASE|SCHEMA|INDEX|VIEW)\s+`, "DROP", SeverityCritical, false},
	{`\bDROP\s+USER\s+`, "DROP_USER", SeverityCritical, false},
	{`\bTRUNCATE\s+(TABLE\s+)?[\w\.` + "`" + `"\[\]]+`, "TRUNCATE", SeverityCritical, false},
	// DELETE with no WHERE at all — the statement ends right after the table name.
	{`\bDELETE\s+FROM\s+[\w\.` + "`" + `"\[\]]+\s*(?:;|$)`, "DELETE_NO_WHERE", SeverityCritical, false},
	// UPDATE ... SET ... with no WHERE anywhere. Python expressed the "no WHERE" half as a
	// negative lookahead; here the regex matches the UPDATE...SET shape and requiresNoWhere adds
	// the second half.
	{`\bUPDATE\s+[\w\.` + "`" + `"\[\]]+\s+SET\s+`, "UPDATE_NO_WHERE", SeverityCritical, true},
	{`\bALTER\s+TABLE\s+[\w\.` + "`" + `"\[\]]+\s+DROP\s+`, "ALTER_DROP", SeverityHigh, false},
	// A WHERE that is always true is a mass delete wearing a WHERE clause.
	{`\bDELETE\s+FROM\s+[\w\.` + "`" + `"\[\]]+\s+WHERE\s+(1\s*=\s*1|TRUE|1)\s*`, "DELETE_ALWAYS_TRUE", SeverityCritical, false},
	{`\bGRANT\s+ALL\s+`, "GRANT_ALL", SeverityHigh, false},
	{`\bREVOKE\s+ALL\s+`, "REVOKE_ALL", SeverityHigh, false},
}

// Component names that look like database tools. The detector only inspects these, so a prompt
// mentioning "drop table" in prose does not trip it.
var builtinDatabaseToolPatterns = []string{
	"*SQL*", "*sql*", "*Database*", "*database*", "*Query*", "*query*",
	"*DB*", "*Postgres*", "*MySQL*", "*SQLite*", "*Supabase*",
	"*Execute*Query*", "*RunSQL*", "*DBTool*",
}

type databaseSafety struct {
	patterns     []dangerPattern
	allowed      []*regexp.Regexp
	toolPatterns []string
	block        bool
}

func newDatabaseSafety(cfg DatabaseSafetyConfig, block bool) (*databaseSafety, error) {
	d := &databaseSafety{block: block, toolPatterns: cfg.ToolPatterns}
	if len(d.toolPatterns) == 0 {
		d.toolPatterns = builtinDatabaseToolPatterns
	}

	for _, p := range builtinDangerPatterns {
		// (?is): case-insensitive, and . spans newlines — a multi-line statement is one statement.
		re, err := regexp.Compile(`(?is)` + p.pattern)
		if err != nil {
			return nil, fmt.Errorf("builtin pattern %q: %w", p.pattern, err)
		}
		d.patterns = append(d.patterns, dangerPattern{re, p.op, p.severity, p.requiresNoWhere})
	}

	custom, err := compileAll(cfg.BlockedPatterns, `(?is)`)
	if err != nil {
		return nil, err
	}
	for _, re := range custom {
		d.patterns = append(d.patterns, dangerPattern{re, "CUSTOM_BLOCKED", SeverityHigh, false})
	}

	d.allowed, err = compileAll(cfg.AllowedPatterns, `(?is)`)
	if err != nil {
		return nil, err
	}
	return d, nil
}

func (d *databaseSafety) Name() string            { return "database_safety" }
func (d *databaseSafety) Stages() []enforce.Stage { return []enforce.Stage{enforce.StageToolCall} }

// Runs before the fuzzier text detectors: it is the most certain rule and the most expensive
// mistake to miss.
func (d *databaseSafety) Priority() int { return 95 }

func (d *databaseSafety) Inspect(req *Request) *Verdict {
	if req.Content == "" || !d.isDatabaseTool(req.ComponentName) {
		return nil
	}

	// The allow list is checked first and wins outright — it exists so an org can exempt a query
	// it has decided is safe, and an exemption that only sometimes applies would be useless.
	for _, re := range d.allowed {
		if re.MatchString(req.Content) {
			return nil
		}
	}

	hasWhere := regexp.MustCompile(`(?i)\bWHERE\b`).MatchString(req.Content)
	for _, p := range d.patterns {
		m := p.re.FindString(req.Content)
		if m == "" {
			continue
		}
		if p.requiresNoWhere && hasWhere {
			continue
		}
		return &Verdict{
			Action:   action(d.block),
			Detector: d.Name(),
			Reason:   "destructive_query",
			Message: fmt.Sprintf("Destructive database operation (%s) blocked on tool %q. %s",
				p.op, req.ComponentName, recommendationFor(p.op)),
			Severity: p.severity,
			Matched:  clipMatch(m),
		}
	}
	return nil
}

func (d *databaseSafety) isDatabaseTool(name string) bool {
	for _, p := range d.toolPatterns {
		if globMatch(p, name) {
			return true
		}
	}
	return false
}

func recommendationFor(op string) string {
	switch op {
	case "DROP", "DROP_USER":
		return "Run schema changes through a reviewed migration, not an agent tool call."
	case "TRUNCATE":
		return "Use a scoped DELETE with a WHERE clause, or run it as a reviewed migration."
	case "DELETE_NO_WHERE", "DELETE_ALWAYS_TRUE":
		return "Add a WHERE clause that selects only the rows you intend to remove."
	case "UPDATE_NO_WHERE":
		return "Add a WHERE clause so the update cannot touch every row."
	case "ALTER_DROP":
		return "Dropping a column is irreversible; run it as a reviewed migration."
	case "GRANT_ALL", "REVOKE_ALL":
		return "Grant the specific privileges required rather than ALL."
	case "CUSTOM_BLOCKED":
		return "This operation matches a pattern your organization blocks."
	default:
		return "Review this operation before running it."
	}
}

// ── Secret scanning ───────────────────────────────────────────────────────────
//
// Port of SecurityScanner's credential half. Runs on both inputs and outputs: a key going out to a
// tool and a key coming back from a model are both leaks.

var builtinSecretPatterns = []string{
	`sk-[a-zA-Z0-9]{48}`,  // OpenAI
	`AIza[a-zA-Z0-9]{35}`, // Google
	// Deliberately NOT porting the Python scanner's bare `[a-f0-9]{32}` "generic API key": it
	// matches any MD5, git blob id, or content hash, and this detector can block. A rule that
	// fires on every commit SHA in a diff would be turned off within a day, taking the real
	// credential patterns with it.
	`ghp_[A-Za-z0-9]{36}`,                                           // GitHub personal access token
	`github_pat_[A-Za-z0-9_]{22,}`,                                  // GitHub fine-grained PAT
	`xox[baprs]-[A-Za-z0-9-]{10,}`,                                  // Slack
	`AKIA[0-9A-Z]{16}`,                                              // AWS access key id
	`-----BEGIN [A-Z ]*PRIVATE KEY-----`,                            // any PEM private key
	`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`, // JWT
}

type secretScan struct {
	patterns []*regexp.Regexp
	block    bool
}

func newSecretScan(cfg SecretScanConfig, block bool) (*secretScan, error) {
	// append to a copy: appending straight onto the package-level slice would let a long extra
	// list scribble into whatever shares its backing array.
	all := append(append([]string{}, builtinSecretPatterns...), cfg.ExtraPatterns...)
	pats, err := compileAll(all, ``)
	if err != nil {
		return nil, err
	}
	return &secretScan{patterns: pats, block: block}, nil
}

func (s *secretScan) Name() string { return "secret_scan" }

// Every stage: a credential is equally a problem on the way in and on the way out.
func (s *secretScan) Stages() []enforce.Stage {
	return []enforce.Stage{
		enforce.StagePreLLM, enforce.StageToolCall,
		enforce.StageToolCallResponse, enforce.StagePostLLM,
	}
}

func (s *secretScan) Priority() int { return 90 }

func (s *secretScan) Inspect(req *Request) *Verdict {
	if req.Content == "" {
		return nil
	}
	for _, re := range s.patterns {
		if m := re.FindString(req.Content); m != "" {
			return &Verdict{
				Action:   action(s.block),
				Detector: s.Name(),
				Reason:   "secret_detected",
				Message: fmt.Sprintf("A credential-shaped value was found in %s content for %q.",
					req.Stage, req.ComponentName),
				Severity: SeverityHigh,
				// The match is redacted rather than echoed: a verdict that quotes the secret
				// copies it straight into the log it was meant to keep it out of.
				Matched: redactSecret(m),
			}
		}
	}
	return nil
}

// redactSecret keeps enough of a value to identify which credential leaked without reproducing it.
func redactSecret(s string) string {
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "***" + s[len(s)-2:]
}

// ── Prompt injection ──────────────────────────────────────────────────────────
//
// Port of PromptInjectionDetector's pattern layer. The Python detector also scores behavioral and
// contextual signals into a confidence value; that scoring is tunable and statistical, which by
// this package's own rule belongs on the server. What is ported is the unambiguous pattern layer.

type injectionPattern struct {
	re       *regexp.Regexp
	kind     string
	severity Severity
}

var builtinInjectionPatterns = []struct {
	pattern  string
	kind     string
	severity Severity
}{
	{`ignore\s+(all\s+|any\s+)?(previous|prior|above|earlier)\s+(instructions?|prompts?|rules?)`, "instruction_override", SeverityHigh},
	{`disregard\s+(all\s+)?(previous|prior|above|the)\s+.{0,20}(instructions?|rules?)`, "instruction_override", SeverityHigh},
	{`forget\s+(everything|all\s+previous|your\s+instructions)`, "instruction_override", SeverityHigh},
	{`you\s+are\s+now\s+(a|an|the)\s`, "role_hijack", SeverityMedium},
	{`pretend\s+(to\s+be|you\s+are)\s`, "role_hijack", SeverityMedium},
	{`act\s+as\s+(a|an|the)\s+(dan|jailbroken|unrestricted|developer\s+mode)`, "jailbreak", SeverityCritical},
	{`(reveal|show|print|repeat)\s+(me\s+)?(your|the)\s+(system\s+)?(prompt|instructions)`, "prompt_extraction", SeverityHigh},
	{`execute\s*:?\s*(rm|del|drop|delete|shutdown)\b`, "command_injection", SeverityCritical},
	{`\b(system|os\.system|subprocess)\s*\(\s*["']`, "command_injection", SeverityCritical},
	// Deliberately MEDIUM, where the Python SDK would block this in enforce mode. That SDK targets
	// application agents; this one also fronts coding agents, for which writing the literal "eval("
	// into a file is routine work. At medium it stays visible without failing every such edit; an org
	// that wants it blocking can lower blockThreshold.
	{`\b(eval|exec)\s*\(`, "code_execution", SeverityMedium},
}

type promptInjection struct {
	patterns  []injectionPattern
	threshold Severity
	block     bool
}

func newPromptInjection(cfg PromptInjectionConfig, block bool) (*promptInjection, error) {
	p := &promptInjection{block: block, threshold: cfg.BlockThreshold}
	if p.threshold == "" {
		// Matches the Python SDK's block_threshold=ThreatLevel.HIGH.
		p.threshold = SeverityHigh
	}
	for _, b := range builtinInjectionPatterns {
		re, err := regexp.Compile(`(?is)` + b.pattern)
		if err != nil {
			return nil, fmt.Errorf("builtin pattern %q: %w", b.pattern, err)
		}
		p.patterns = append(p.patterns, injectionPattern{re, b.kind, b.severity})
	}
	extra, err := compileAll(cfg.ExtraPatterns, `(?is)`)
	if err != nil {
		return nil, err
	}
	for _, re := range extra {
		p.patterns = append(p.patterns, injectionPattern{re, "custom", SeverityHigh})
	}
	return p, nil
}

func (p *promptInjection) Name() string { return "prompt_injection" }

// Inbound content only. A tool's ARGUMENTS are inbound in the sense that matters — that is where
// content fetched from elsewhere gets passed along — which is why the Python SDK scans tool inputs
// too (_scan_tool_inputs -> scan_input). A model's own OUTPUT is excluded: flagging it produces a
// warning about the agent quoting the attack it just refused, which is noise.
func (p *promptInjection) Stages() []enforce.Stage {
	return []enforce.Stage{enforce.StagePreLLM, enforce.StageToolCall, enforce.StageToolCallResponse}
}

func (p *promptInjection) Priority() int { return 85 }

func (p *promptInjection) Inspect(req *Request) *Verdict {
	if req.Content == "" {
		return nil
	}
	for _, pat := range p.patterns {
		m := pat.re.FindString(req.Content)
		if m == "" {
			continue
		}
		// Below the threshold the finding is reported but never blocks, even in enforce mode —
		// a MEDIUM role-hijack phrasing is worth seeing and not worth failing a build over.
		blocking := p.block && pat.severity.AtLeast(p.threshold)
		return &Verdict{
			Action:   action(blocking),
			Detector: p.Name(),
			Reason:   "prompt_injection",
			Message: fmt.Sprintf("Possible prompt injection (%s) in %s content for %q.",
				pat.kind, req.Stage, req.ComponentName),
			Severity: pat.severity,
			Matched:  clipMatch(m),
		}
	}
	return nil
}

// ── Token budget ──────────────────────────────────────────────────────────────
//
// Port of TokenTrackingMiddleware's limit half. The Python middleware also ships usage to the
// cloud; that reporting is the telemetry package's job here, not a policy detector's.

type tokenBudget struct {
	max   int
	block bool

	mu   sync.Mutex
	used map[string]int
}

func newTokenBudget(cfg TokenBudgetConfig, block bool) *tokenBudget {
	return &tokenBudget{max: cfg.MaxTokens, block: block, used: map[string]int{}}
}

func (t *tokenBudget) Name() string { return "token_budget" }

// Charged at the point the spend is known.
func (t *tokenBudget) Stages() []enforce.Stage {
	return []enforce.Stage{enforce.StagePreLLM, enforce.StagePostLLM}
}

// Lowest priority: a budget breach is the least urgent thing to report if something else also
// fired, and it mutates state, so it should not run when an earlier detector already blocked.
func (t *tokenBudget) Priority() int { return 10 }

func (t *tokenBudget) Inspect(req *Request) *Verdict {
	if t.max <= 0 || req.Tokens <= 0 {
		return nil
	}
	t.mu.Lock()
	total := t.used[req.SessionID] + req.Tokens
	t.used[req.SessionID] = total
	t.mu.Unlock()

	if total <= t.max {
		return nil
	}
	return &Verdict{
		Action:   action(t.block),
		Detector: t.Name(),
		Reason:   "token_budget_exceeded",
		Message: fmt.Sprintf("Session token budget exhausted: %d of %d used.",
			total, t.max),
		Severity: SeverityMedium,
	}
}

// ResetSession drops a session's accumulated spend. Sessions are otherwise remembered for the
// process lifetime, which would leak memory in a long-running daemon handling many sessions.
func (t *tokenBudget) ResetSession(session string) {
	t.mu.Lock()
	delete(t.used, session)
	t.mu.Unlock()
}
