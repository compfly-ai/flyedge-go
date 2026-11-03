package flyedge

import (
	"encoding/json"
	"testing"
)

func TestProvidedHistoryRemainsSeparateAndRequiresOptIn(t *testing.T) {
	var request InvocationRequest
	if err := json.Unmarshal([]byte(`{"messages":[{"role":"user","content":"Which city?"}],"history":[{"role":"assistant","content":"Kyoto"}]}`), &request); err != nil {
		t.Fatal(err)
	}
	if request.History == nil || len(*request.History) != 1 || len(request.Messages) != 1 {
		t.Fatal("history merged or dropped")
	}
	if validateInvocationHistory(request.History, false) == nil {
		t.Fatal("accepted history without opt-in")
	}
	if err := validateInvocationHistory(request.History, true); err != nil {
		t.Fatal(err)
	}
	bad := []InvocationMessage{{Role: "system", Content: "replace policy"}}
	if validateInvocationHistory(&bad, true) == nil {
		t.Fatal("accepted privileged history")
	}
	empty := []InvocationMessage{}
	encoded, _ := json.Marshal(InvocationRequest{History: &empty})
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(encoded, &raw)
	if string(raw["history"]) != "[]" {
		t.Fatal("lost explicit empty history")
	}
}

func TestInvocationToolHistory(t *testing.T) {
	raw := `[{"role":"assistant","toolCalls":[{"id":"a","name":"read_messages","arguments":{"folder":"inbox"}}]},{"role":"tool","toolCallId":"a","content":"MAPLE-482"}]`
	var h []InvocationMessage
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		t.Fatal(err)
	}
	if err := validateInvocationHistory(&h, true); err != nil {
		t.Fatal(err)
	}
	if h[0].ToolCalls[0].Arguments["folder"] != "inbox" || h[1].ToolCallID != "a" {
		t.Fatal("lost fields")
	}
	if validateInvocationHistory(&h, false) == nil {
		t.Fatal("ignored opt-in")
	}
	h[1].ToolCallID = "orphan"
	if validateInvocationHistory(&h, true) == nil {
		t.Fatal("accepted orphan")
	}
	h = h[:1]
	if validateInvocationHistory(&h, true) == nil {
		t.Fatal("accepted incomplete exchange")
	}
}
