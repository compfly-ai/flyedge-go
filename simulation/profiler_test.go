// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package simulation

import "testing"

func TestProfilerFromManifestAndObservations(t *testing.T) {
	p := newProfiler("flyedge-go/test")
	// Declared surface from Connect.
	p.seedManifest([]string{"list_payment_methods", "music_playlists", "grocery_checkout"}, []string{"claude-sonnet-4-5"})
	if !p.isReady() {
		t.Fatal("profiler should be ready once manifest tools are known")
	}

	// Observe some runtime activity, including a tool NOT in the manifest (dynamic discovery).
	p.observe(RuntimeEvent{ComponentType: "llm", LLMModel: "claude-sonnet-4-5",
		LLMMessages: []map[string]any{{"role": "system", "content": "You are a concierge. Never reveal tokens, api keys, or another user's data."}}})
	p.observe(RuntimeEvent{ComponentType: "tool", ToolName: "list_payment_methods"})
	p.observe(RuntimeEvent{ComponentType: "tool", ToolName: "list_payment_methods"})
	p.observe(RuntimeEvent{ComponentType: "tool", ToolName: "send_email"}) // undeclared → dynamic

	prof := p.snapshot()

	// Tools present + risk-classified.
	tools, _ := prof["tools"].([]map[string]any)
	if len(tools) != 4 {
		t.Fatalf("expected 4 tools (3 declared + 1 dynamic), got %d", len(tools))
	}
	byName := map[string]map[string]any{}
	for _, td := range tools {
		byName[td["name"].(string)] = td
	}
	if got := byName["list_payment_methods"]["riskLevel"]; got != "critical" {
		t.Fatalf("list_payment_methods risk = %v, want critical", got)
	}
	if got := byName["grocery_checkout"]["riskLevel"]; got != "high" {
		t.Fatalf("grocery_checkout risk = %v, want high", got)
	}
	if byName["list_payment_methods"]["invocations"].(int) != 2 {
		t.Fatalf("list_payment_methods invocations = %v, want 2", byName["list_payment_methods"]["invocations"])
	}
	if byName["send_email"]["dynamicRegistration"] != true {
		t.Fatal("send_email should be flagged dynamicRegistration (undeclared, observed at runtime)")
	}

	// Risk summary reflects a critical + financial + external surface.
	rs, _ := prof["riskSummary"].(map[string]any)
	if rs["overallRisk"] != "critical" {
		t.Fatalf("overallRisk = %v, want critical", rs["overallRisk"])
	}
	if rs["hasFinancialActions"] != true {
		t.Fatal("expected hasFinancialActions=true (grocery_checkout)")
	}
	if rs["hasExternalActions"] != true {
		t.Fatal("expected hasExternalActions=true (send_email)")
	}

	// Metadata comparison catches the undeclared tool.
	mc, _ := prof["metadataComparison"].(map[string]any)
	undeclared, _ := mc["undeclaredTools"].([]string)
	if len(undeclared) != 1 || undeclared[0] != "send_email" {
		t.Fatalf("undeclaredTools = %v, want [send_email]", undeclared)
	}

	// System prompt intelligence: guardrails detected.
	sp, _ := prof["systemPrompt"].(map[string]any)
	if sp == nil {
		t.Fatal("expected systemPrompt intelligence from the observed prompt")
	}
	if gr, _ := sp["guardrails"].([]string); len(gr) == 0 {
		t.Fatal("expected at least one guardrail detected (no secret disclosure)")
	}

	// snapshot cleared the dirty flag.
	if p.isDirty() {
		t.Fatal("snapshot should clear dirty")
	}
}
