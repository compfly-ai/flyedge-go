package simulation

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInjectorExplicitChains(t *testing.T) {
	inj := newInjector()
	cfg := `{
		"mode":"attack","tier":2,"max_injections":10,
		"attack_config":{"chains":[
			{"name":"poison_pay","steps":[{"strategy":"tool_poison","target_component_type":"tool","target_component_name":"list_payment_methods","sophistication":2}]},
			{"name":"err_checkout","steps":[{"strategy":"error_inject","target_component_type":"tool","target_component_name":"grocery_checkout","sophistication":1}]},
			{"name":"cfg","steps":[{"strategy":"config_inject","target_component_type":"llm","target_component_name":"*","sophistication":1}]}
		]}
	}`
	inj.configure(json.RawMessage(cfg), nil)
	if !inj.attackActive() {
		t.Fatal("injector should be attack-active")
	}

	// tool_poison merges into a JSON object result.
	out, meta, ok := inj.injectForTool("list_payment_methods", `{"paymentMethods":[{"brand":"Visa"}]}`)
	if !ok {
		t.Fatal("expected tool_poison to fire on list_payment_methods")
	}
	if meta.Strategy != "tool_poison" || meta.Target != "list_payment_methods" || meta.Tier != 2 {
		t.Fatalf("unexpected meta: %+v", meta)
	}
	if !strings.Contains(out, "admin_override") && !strings.Contains(out, "escalation_note") {
		t.Fatalf("poisoned result missing payload: %s", out)
	}
	// original content preserved (merged, not replaced)
	if !strings.Contains(out, "Visa") {
		t.Fatalf("tool_poison should merge, not replace: %s", out)
	}

	// error_inject replaces the result with a crafted error.
	out2, meta2, ok := inj.injectForTool("grocery_checkout", `{"status":"ok"}`)
	if !ok || meta2.Strategy != "error_inject" {
		t.Fatalf("expected error_inject on grocery_checkout, got ok=%v meta=%+v", ok, meta2)
	}
	if strings.Contains(out2, "status") {
		t.Fatalf("error_inject should replace the result, got: %s", out2)
	}

	// config_inject yields a system message for the LLM path.
	msg, meta3, ok := inj.injectForLLM("api.anthropic.com")
	if !ok || meta3.Strategy != "config_inject" || msg == "" {
		t.Fatalf("expected config_inject, got ok=%v meta=%+v msg=%q", ok, meta3, msg)
	}

	// A tool with no matching chain does not inject.
	if _, _, ok := inj.injectForTool("music_playlists", "{}"); ok {
		t.Fatal("music_playlists has no chain — must not inject")
	}
}

func TestInjectorDefaultChainsFromProfile_AndBudget(t *testing.T) {
	profile := map[string]any{
		"tools": []map[string]any{
			{"name": "grocery_checkout", "riskLevel": "high"},
		},
	}
	inj := newInjector()
	// No explicit chains → default chains built from the profile; cap injections at 1.
	inj.configure(json.RawMessage(`{"mode":"attack","tier":3,"max_injections":1,"attack_config":{"sophistication_range":[1,1]}}`), profile)

	// One injection allowed, then budget exhausted.
	if _, _, ok := inj.injectForTool("grocery_checkout", `{"ok":true}`); !ok {
		t.Fatal("expected first injection to fire (default tool_poison/error_inject on grocery_checkout)")
	}
	if inj.attackActive() {
		t.Fatal("budget of 1 should be exhausted after one injection")
	}
	if _, _, ok := inj.injectForTool("grocery_checkout", `{"ok":true}`); ok {
		t.Fatal("no injection should fire past max_injections")
	}
}

func TestInjectorObserveModeInert(t *testing.T) {
	inj := newInjector()
	inj.configure(json.RawMessage(`{"mode":"observe"}`), nil)
	if inj.attackActive() {
		t.Fatal("observe mode must not be attack-active")
	}
	if _, _, ok := inj.injectForTool("anything", "{}"); ok {
		t.Fatal("observe mode must not inject")
	}
}

func TestResolvePayloadFillsPlaceholders(t *testing.T) {
	profile := map[string]any{
		"systemPrompt": map[string]any{"purpose": "banking concierge"},
		"tools":        []map[string]any{{"name": "wire_transfer"}},
	}
	p := resolvePayload("config_inject", 2, profile, 0)
	s, _ := p.(string)
	if !strings.Contains(s, "banking concierge") {
		t.Fatalf("expected {purpose} filled from profile, got: %s", s)
	}
}
