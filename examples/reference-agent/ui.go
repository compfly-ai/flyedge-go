// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/compfly-ai/flyedge-go"
)

// Light ANSI formatting for the REPL (role labels, per-turn governed step trace) and the
// governance event log. Same palette as CompFly's internal reference agent.
const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
)

// gov pretty-logs one governance event — a moment the platform acts on the agent — to stderr, so
// events show up in both the CLI and -serve modes without corrupting a stdout response body.
func gov(color, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "  "+color+format+reset+"\n", args...)
}

// banner prints the identity + governance header shared by the chat, one-off, and serve modes.
func banner(g *flyedge.Guard, p provider, u *user) {
	fmt.Printf("%s%sflyedge reference agent%s  %sacting for %s <%s> plan=%s%s\n",
		bold, cyan, reset, dim, u.Name, u.Email, u.Plan, reset)
	if did := g.DID(); did != "" {
		fmt.Printf("%sgoverned by flyedge — DID %s · gateway %s · mode %s · model %s (%s)%s\n",
			dim, did, envOr("COMPFLY_API_URL", "(unset)"), envOr("FLYEDGE_MODE", "warn"),
			p.Model(), p.Name(), reset)
	} else {
		fmt.Printf("%sflyedge unsigned (no DID) — checks fail-open locally · model %s (%s)%s\n",
			dim, p.Model(), p.Name(), reset)
	}
}

// printReply renders the governed step trace + the assistant answer.
func printReply(r *reply) {
	for _, s := range r.steps {
		args := s.args
		if args == "{}" || args == "" {
			args = ""
		}
		if s.allowed {
			fmt.Printf("%s  ↳ %s(%s) ✓%s\n", dim, s.tool, args, reset)
		} else {
			fmt.Printf("%s  ↳ %s(%s) ✗ blocked: %s%s\n", yellow, s.tool, args, s.reason, reset)
		}
	}
	answer := strings.TrimSpace(r.text)
	if answer == "" {
		answer = "(no text)"
	}
	fmt.Printf("%sagent ▸%s %s\n", green, reset, answer)
}
