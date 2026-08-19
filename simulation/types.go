// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Package simulation implements the flyedge-go client for the platform's agent
// simulation / attack-injection layer. When the config poller reports an active
// simulation (the `simulation` block of GET /v1/flyedge/config), the Controller
// streams RuntimeEvents to the run's telemetry WebSocket so the evaluation
// harness (agent-eval) can observe — and, in attack mode, red-team — the agent.
//
// It is a subpackage on purpose: the WebSocket dependency lives here only, so the
// core flyedge package stays stdlib-only. flyedge imports simulation (one-way);
// simulation never imports flyedge.
//
// Behavioral reference: the Python SDK's flyedge/simulation/ (types, ws_transport,
// config_handler, behavior_monitor). The wire shapes below match it exactly.
package simulation

import (
	"encoding/json"
	"time"
)

// SystemPromptID is the sentinel prompt_id for system-level events (heartbeat).
const SystemPromptID = "__system__"

// State is the lifecycle state of the simulation controller.
type State string

const (
	StateInactive State = "inactive"
	StateStarting State = "starting"
	StateActive   State = "active"
	StateStopping State = "stopping"
)

// Config is the `simulation` block from GET /v1/flyedge/config (frozen wire —
// matches prism SimulationConfig / the Python SimulationConfig). flyedge converts
// its own SimulationConfig into this when handing off to the Controller.
type Config struct {
	Active             bool            `json:"active"`
	RunID              string          `json:"run_id"`
	Middlewares        []string        `json:"middlewares"`
	TelemetryJWT       string          `json:"telemetry_jwt"`
	TelemetryURL       string          `json:"telemetry_url"`
	ProtectionDisabled bool            `json:"protection_disabled"`
	Extra              json.RawMessage `json:"extra,omitempty"`
}

// Valid reports whether the config has everything required to activate a run.
func (c *Config) Valid() bool {
	return c != nil && c.Active && c.RunID != "" && c.TelemetryJWT != "" && c.TelemetryURL != ""
}

// HasMiddleware reports whether name is in the server-selected middleware list.
func (c *Config) HasMiddleware(name string) bool {
	if c == nil {
		return false
	}
	for _, m := range c.Middlewares {
		if m == name {
			return true
		}
	}
	return false
}

// RuntimeEvent is a single runtime event streamed over the telemetry WebSocket.
// prism republishes it to Redis sim:telemetry:{run_id} for the eval harness.
// JSON matches the Python RuntimeEvent.to_dict (optional fields omitted when empty).
type RuntimeEvent struct {
	EventID       string  `json:"event_id"`
	RunID         string  `json:"run_id"`
	PromptID      string  `json:"prompt_id"`
	Timestamp     float64 `json:"timestamp"` // unix epoch seconds (float), matching Python time.time()
	ComponentType string  `json:"component_type"`
	ComponentName string  `json:"component_name"`
	Framework     string  `json:"framework,omitempty"`

	// LLM-specific
	LLMMessages  []map[string]any `json:"llm_messages,omitempty"`
	LLMModel     string           `json:"llm_model,omitempty"`
	LLMResponse  string           `json:"llm_response,omitempty"`
	LLMToolCalls []map[string]any `json:"llm_tool_calls,omitempty"`

	// Tool-specific
	ToolName   string `json:"tool_name,omitempty"`
	ToolArgs   any    `json:"tool_args,omitempty"` // dict (map) or the heartbeat payload
	ToolResult string `json:"tool_result,omitempty"`
	ToolError  string `json:"tool_error,omitempty"`

	// Retriever-specific
	RetrieverQuery   string           `json:"retriever_query,omitempty"`
	RetrieverResults []map[string]any `json:"retriever_results,omitempty"`

	// Behavioral flags (set by the behavior monitor)
	Flags []string `json:"flags,omitempty"`

	// Injection tracking (set by the attack injector — Phase B2)
	InjectionID            string `json:"injection_id,omitempty"`
	InjectionStrategy      string `json:"injection_strategy,omitempty"`
	InjectionTarget        string `json:"injection_target,omitempty"`
	InjectionSophistication int   `json:"injection_sophistication,omitempty"`
	InjectionChain         string `json:"injection_chain,omitempty"`
	InjectionTier          int    `json:"injection_tier,omitempty"`

	// Agent profiling (set by the attack injector in observe mode — Phase B2)
	AgentProfile map[string]any `json:"agent_profile,omitempty"`
}

// nowUnix returns the current time as float epoch seconds (Python time.time() shape).
func nowUnix() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}
