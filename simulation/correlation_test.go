package simulation

import "testing"

func TestPromptCorrelationPreservesSessionAndColonInPrompt(t *testing.T) {
	ev := RuntimeEvent{PromptID: "eval:run-1:redteam.tool:lookup.001"}
	correlateEvent(&ev)
	if ev.SessionID != "eval:run-1:redteam.tool:lookup.001" || ev.PromptID != "redteam.tool:lookup.001" {
		t.Fatalf("bad correlation: %+v", ev)
	}
	plain := RuntimeEvent{PromptID: "ordinary-session"}
	correlateEvent(&plain)
	if plain.PromptID != "ordinary-session" {
		t.Fatal(plain)
	}
}
