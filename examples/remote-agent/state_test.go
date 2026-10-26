package main

import (
	"encoding/json"
	"testing"
)

func TestOrderFromState(t *testing.T) {
	for _, tc := range []struct {
		raw, id, status string
		invalid         bool
	}{
		{"null", "DEMO-42", "shipped", false},
		{`{"selected_order_id":"DEMO-43"}`, "DEMO-43", "processing", false},
		{`{"selected_order_id":null,"form":{"order_id":"DEMO-42","include_tracking":false}}`, "DEMO-42", "shipped", false},
		{`{}`, "", "", true},
		{`{"selected_order_id":42}`, "", "", true},
		{`{"selected_order_id":"OTHER"}`, "", "", true},
	} {
		var state map[string]json.RawMessage
		if err := json.Unmarshal([]byte(tc.raw), &state); err != nil {
			t.Fatal(err)
		}
		id, status, err := orderFromState(state)
		if (err != nil) != tc.invalid || id != tc.id || status != tc.status {
			t.Fatalf("%s: %q %q %v", tc.raw, id, status, err)
		}
	}
}
