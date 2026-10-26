package main

import (
	"encoding/json"
	"fmt"
)

// Client context selects a read-only fixture; it never grants permissions.
func orderFromState(state map[string]json.RawMessage) (string, string, error) {
	id := "DEMO-42"
	if state != nil {
		id = ""
		if raw, ok := state["selected_order_id"]; ok {
			if err := json.Unmarshal(raw, &id); err != nil {
				return "", "", fmt.Errorf("selected_order_id must be a string or null")
			}
		}
		if id == "" {
			var form struct {
				OrderID string `json:"order_id"`
			}
			if raw, ok := state["form"]; ok {
				if err := json.Unmarshal(raw, &form); err != nil {
					return "", "", fmt.Errorf("form must contain a string order_id")
				}
			}
			id = form.OrderID
		}
		if id == "" {
			return "", "", fmt.Errorf("client state must select an order")
		}
	}
	switch id {
	case "DEMO-42":
		return id, "shipped", nil
	case "DEMO-43":
		return id, "processing", nil
	default:
		return "", "", fmt.Errorf("unknown fixture order")
	}
}
