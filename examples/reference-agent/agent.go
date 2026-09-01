// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/compfly-ai/flyedge-go"
)

// agent is the governed, provider-agnostic tool-use loop. The provider owns the SDK-specific loop;
// the agent owns tool gating: every tool the model requests passes CheckToolCall before execution
// and GovernToolResult on its result. A policy denial becomes a tool_result error the model sees,
// so the agent adapts under policy instead of crashing.
type agent struct {
	guard    *flyedge.Guard
	provider provider
}

func newAgent(g *flyedge.Guard, hc *http.Client, providerName, model string) (*agent, error) {
	p, err := newProvider(hc, providerName, model)
	if err != nil {
		return nil, err
	}
	return &agent{guard: g, provider: p}, nil
}

// step is one governed action in a turn — for the REPL's per-turn trace (allowed vs policy-blocked).
type step struct {
	tool, args string
	allowed    bool
	reason     string // deny/error reason when !allowed
}

// reply is the agent's response to one user message: the final text plus the governed step trace.
type reply struct {
	text  string
	steps []step
}

// handle runs one user message through the governed tool-use loop.
func (a *agent) handle(ctx context.Context, u *user, session, userText string) (*reply, error) {
	// Attach the session and the on-behalf-of principal so EVERY governed call in this turn — the
	// model round-trips through the wrapped transport AND each CheckToolCall/GovernToolResult —
	// carries them. This is what lets one agent identity be governed per end user.
	ctx = flyedge.ContextWithSession(ctx, session)
	ctx = flyedge.ContextWithPrincipal(ctx, principalFor(u))

	// exec gates + executes + governs one tool call; the provider's loop calls it per tool_use.
	exec := func(ctx context.Context, name string, args json.RawMessage) (string, bool, step) {
		return a.guardedTool(ctx, session, u, name, args)
	}

	text, steps, err := a.provider.Run(ctx, systemPrompt(u), userText, toolDefs, exec)
	if err != nil {
		// A pre_llm or post_llm denial arrives here as a typed error — the turn ends with a clear
		// outcome instead of a crash.
		if de, ok := flyedge.AsDenyError(err); ok {
			return &reply{text: "[model call blocked by policy: " + de.Decision.Reason + "]", steps: steps}, nil
		}
		if ke, ok := flyedge.AsKillSwitchError(err); ok {
			return &reply{text: "[blocked: kill switch active: " + killReason(ke.Kills) + "]", steps: steps}, nil
		}
		return nil, err
	}
	return &reply{text: text, steps: steps}, nil
}

// guardedTool is the full tool governance sequence: gate the CALL, execute, then govern the RESULT
// before it re-enters the model's context. Denials become tool results (isErr=true) the model can
// adapt to. Governance events are pretty-logged to stderr (see ui.go), so they show up in both the
// CLI and -serve modes without corrupting a response body.
func (a *agent) guardedTool(ctx context.Context, session string, u *user, name string, args json.RawMessage) (string, bool, step) {
	s := step{tool: name, args: string(args)}
	def, ok := toolsByName[name]
	if !ok {
		s.reason = "unknown tool"
		return "unknown tool: " + name, true, s
	}
	gov(dim, "→ tool_call: %s %s", name, string(args))

	// Gate 1 — tool_call, BEFORE execution.
	dec, err := a.guard.CheckToolCall(ctx, session, name, string(args), def.dest(args))
	if err != nil {
		if de, ok := flyedge.AsDenyError(err); ok {
			gov(red, "  🛡  DENIED: %s — not executed", de.Decision.Reason)
			s.reason = de.Decision.Reason
			return "blocked by security policy: " + denyText(de), true, s
		}
		if ke, ok := flyedge.AsKillSwitchError(err); ok {
			gov(red, "  🛡  KILL SWITCH: %s — not executed", killReason(ke.Kills))
			s.reason = "kill switch"
			return "blocked: kill switch active", true, s
		}
		gov(yellow, "  ⚠  check error: %v", err)
		s.reason = err.Error()
		return "policy check error: " + err.Error(), true, s
	}
	switch {
	case dec.Reason == "fail_open":
		// A fail-OPEN allow is NOT enforcement — never hide it behind a green "allowed".
		gov(yellow, "  ⚠  enforcement UNREACHABLE — failed OPEN, not a policy allow")
	case dec.Action == flyedge.ActionWarn:
		gov(yellow, "  🛡  WARN (%s) — executing", dec.Reason)
	default:
		gov(green, "  🛡  allowed — executing")
	}

	result := def.run(u, args)

	// Gate 2 — tool_call_response: govern the result before the model sees it. The returned value
	// may differ from the raw result (redaction, or injection during an attack simulation) — always
	// use the governed value.
	governed, _, gerr := a.guard.GovernToolResult(ctx, session, name, result)
	if gerr != nil {
		if de, ok := flyedge.AsDenyError(gerr); ok {
			gov(red, "  🛡  response DENIED: %s — withheld from the model", de.Decision.Reason)
			s.reason = "response " + de.Decision.Reason
			return "result withheld by security policy: " + denyText(de), true, s
		}
		gov(yellow, "  ⚠  response check error: %v", gerr)
	}
	if governed != result {
		gov(cyan, "  🛡  response transformed before re-entering context")
	}
	gov(dim, "  result: %s", governed)
	s.allowed = true
	return governed, false, s
}

// denyText is what the model gets to read about a denial. Prefer the control's authored
// deny message (Decision.Message — the platform's prose about what happened and what to do
// next) over the bare machine reason code, which reads as a transient error and invites a
// retry.
func denyText(de *flyedge.DenyError) string {
	if de.Decision.Message != "" {
		return de.Decision.Reason + " — " + de.Decision.Message
	}
	return de.Decision.Reason
}

func killReason(kills []flyedge.KillInfo) string {
	if len(kills) > 0 {
		return kills[0].Reason
	}
	return "active"
}

// --- the acting users (a stand-in for your real identity layer) -------------------------------

type user struct {
	ID, Name, Email, Plan string
}

var users = map[string]*user{
	"alice": {ID: "alice", Name: "Alice Nguyen", Email: "alice@example.com", Plan: "pro"},
	"bob":   {ID: "bob", Name: "Bob Marsh", Email: "bob@example.com", Plan: "free"},
}

// principalFor maps the acting user to the flyedge on-behalf-of envelope. OBOID/UPN identify the
// user (audit/attribution); the claims in Scope are what attribute-based policy keys on
// (obo.scope.plan == "free" → deny), independent of how many users exist. In production these come
// from the caller's verified identity token.
func principalFor(u *user) flyedge.Principal {
	return flyedge.Principal{
		Provider: "mock",
		OBOID:    u.ID,
		UPN:      u.Email,
		URN:      "compfly:identity:v1:mock:user:plan=" + u.Plan,
		Scope:    map[string]string{"plan": u.Plan},
	}
}

func systemPrompt(u *user) string {
	return fmt.Sprintf("You are a personal concierge acting on behalf of %s (%s, plan %s). "+
		"Use the available tools to complete the user's request end to end, then summarize the "+
		"outcome — including any actions that were blocked by policy. Be concise.",
		u.Name, u.Email, u.Plan)
}

// --- the agent's tools -------------------------------------------------------------------------

// toolDef is one mock tool: a provider-neutral schema for the model, a dest for the tool_call
// policy (a service name or external host — empty for purely local tools), and the implementation.
type toolDef struct {
	name, description string
	properties        map[string]any
	required          []string
	dest              func(args json.RawMessage) string
	run               func(u *user, args json.RawMessage) string
}

var toolDefs = []toolDef{
	{
		name:        "get_profile",
		description: "Get the acting user's profile: name, email, plan.",
		properties:  map[string]any{},
		dest:        func(json.RawMessage) string { return "" },
		run: func(u *user, _ json.RawMessage) string {
			return fmt.Sprintf("name=%s email=%s plan=%s", u.Name, u.Email, u.Plan)
		},
	},
	{
		name:        "send_payment",
		description: "Send a payment from the user's account to a recipient.",
		properties: map[string]any{
			"to":         map[string]any{"type": "string", "description": "recipient email"},
			"amount_usd": map[string]any{"type": "number", "description": "amount in USD"},
		},
		required: []string{"to", "amount_usd"},
		// The dest names the SERVICE the tool touches — what a service-destination policy matches.
		dest: func(json.RawMessage) string { return "payments" },
		run: func(u *user, args json.RawMessage) string {
			a := argMap(args)
			// The confirmation deliberately carries a credential-shaped token: material a
			// tool_call_response policy (or local detector) can redact/deny before the model sees it.
			return fmt.Sprintf("payment of $%v to %v sent from %s's account; auth_token=tok_%s_9f3acde114",
				a["amount_usd"], a["to"], u.ID, u.ID)
		},
	},
	{
		name:        "fetch_url",
		description: "Fetch the contents of an external URL over HTTP.",
		properties: map[string]any{
			"url": map[string]any{"type": "string", "description": "the URL to fetch"},
		},
		required: []string{"url"},
		// The dest is the external HOST — what an egress policy allows or denies.
		dest: func(args json.RawMessage) string {
			raw, _ := argMap(args)["url"].(string)
			if p, err := url.Parse(raw); err == nil {
				return p.Host
			}
			return ""
		},
		run: func(_ *user, args json.RawMessage) string {
			// Only reached when policy ALLOWED the egress; a demo needs no real network call.
			return fmt.Sprintf("(fetched %v: 3 offers — 20%% off shipping, free returns, 2-for-1 coffee)",
				argMap(args)["url"])
		},
	},
}

var toolsByName = func() map[string]toolDef {
	m := make(map[string]toolDef, len(toolDefs))
	for _, d := range toolDefs {
		m[d.name] = d
	}
	return m
}()

func toolNames() []string {
	names := make([]string, len(toolDefs))
	for i, d := range toolDefs {
		names[i] = d.name
	}
	return names
}

func argMap(args json.RawMessage) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(args, &m)
	return m
}
