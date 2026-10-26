package flyedge

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// InvocationContract declares logical input state, independent of endpoint mappings.
type InvocationContract struct {
	State StateContract `json:"state"`
}
type StateContract struct {
	Fields   map[string]StateField `json:"fields"`
	Required []string              `json:"required,omitempty"`
}
type StateFieldType string

const (
	StateString  StateFieldType = "string"
	StateNumber  StateFieldType = "number"
	StateBoolean StateFieldType = "boolean"
	StateJSON    StateFieldType = "json"
)

type StateField struct {
	Type StateFieldType `json:"type"`
	// PlaygroundDefault prefills the Playground editor only. It never fills runtime or scenario state.
	PlaygroundDefault json.RawMessage `json:"playground_default,omitempty"`
}

var stateFieldName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func snapshotInvocationContract(c *InvocationContract) (*InvocationContract, error) {
	if c == nil {
		return nil, nil
	}
	if c.State.Fields == nil || len(c.State.Fields) > 64 {
		return nil, fmt.Errorf("flyedge: contract requires fields (maximum 64)")
	}
	for name, field := range c.State.Fields {
		if len(name) > 128 || !stateFieldName.MatchString(name) {
			return nil, fmt.Errorf("flyedge: invalid state field name")
		}
		if len(field.PlaygroundDefault) > 0 {
			var value any
			if err := json.Unmarshal(field.PlaygroundDefault, &value); err != nil {
				return nil, fmt.Errorf("flyedge: invalid PlaygroundDefault JSON for %s", name)
			}
			valid := field.Type == StateJSON
			switch field.Type {
			case StateString:
				_, valid = value.(string)
			case StateNumber:
				_, valid = value.(float64)
			case StateBoolean:
				_, valid = value.(bool)
			}
			if !valid {
				return nil, fmt.Errorf("flyedge: PlaygroundDefault for %s must be %s", name, field.Type)
			}
		}
		switch field.Type {
		case StateString, StateNumber, StateBoolean, StateJSON:
		default:
			return nil, fmt.Errorf("flyedge: invalid state type for %s", name)
		}
	}
	seen := map[string]bool{}
	for _, name := range c.State.Required {
		if _, ok := c.State.Fields[name]; !ok || seen[name] {
			return nil, fmt.Errorf("flyedge: invalid required state field %s", name)
		}
		seen[name] = true
	}
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var copy InvocationContract
	err = json.Unmarshal(data, &copy)
	return &copy, err
}
