// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"encoding/json"

	"github.com/compfly-ai/flyedge-go/simulation"
)

// toSimConfig converts the poller's SimulationConfig (the flyedge wire struct) into the simulation
// package's Config. Returns nil for nil (no active run).
func (g *Guard) toSimConfig(sc *SimulationConfig) *simulation.Config {
	if sc == nil {
		return nil
	}
	// Split-horizon override: when the caller pinned a telemetry URL (host-run agent vs in-cluster
	// gateway), the controller dials that instead of the server-advertised one. SimulationConfig()
	// still returns the honest server view — only the controller's transport is redirected.
	telemetryURL := sc.TelemetryURL
	if g.cfg.SimTelemetryURL != "" {
		telemetryURL = g.cfg.SimTelemetryURL
	}
	return &simulation.Config{
		Active:             sc.Active,
		RunID:              sc.RunID,
		Middlewares:        sc.Middlewares,
		TelemetryJWT:       sc.TelemetryJWT,
		TelemetryURL:       telemetryURL,
		ProtectionDisabled: sc.ProtectionDisabled,
		Extra:              sc.Extra,
	}
}

// recordSimEvent maps a CheckRequest into a simulation RuntimeEvent (+ behavior input) and hands it
// to the controller. Called from Check while a run is active; the controller no-ops unless the
// telemetry middleware is enabled. This is how the Guard's existing seams (transport pre/post_llm,
// tool_call, tool_call_response) become the simulation's observation points — no separate
// instrumentation.
func (g *Guard) recordSimEvent(req CheckRequest) {
	ev := simulation.RuntimeEvent{PromptID: req.SessionID, ComponentName: req.ComponentName}
	var bi simulation.BehaviorInput
	switch req.Stage {
	case StageToolCall:
		ev.ComponentType = "tool"
		ev.ToolName = req.Operation.ToolName
		ev.ToolArgs = jsonAnyOrString(req.Content.Full)
		bi = simulation.BehaviorInput{ComponentType: "tool", ComponentName: req.ComponentName, ArgsText: req.Content.Full}
	case StageToolCallResponse:
		ev.ComponentType = "tool"
		ev.ToolName = req.Operation.ToolName
		ev.ToolResult = simulation.Truncate(req.Content.Full)
		bi = simulation.BehaviorInput{ComponentType: "tool", ComponentName: req.ComponentName, ResultText: req.Content.Full}
	case StagePreLLM:
		ev.ComponentType = "llm"
		ev.LLMModel = req.Operation.ModelID
		ev.LLMMessages = []map[string]any{{"role": "user", "content": simulation.Truncate(req.Content.Full)}}
		bi = simulation.BehaviorInput{ComponentType: "llm", ComponentName: req.ComponentName, ArgsText: req.Content.Full}
	case StagePostLLM:
		ev.ComponentType = "llm"
		ev.LLMModel = req.Operation.ModelID
		ev.LLMResponse = simulation.Truncate(req.Content.Full)
		bi = simulation.BehaviorInput{ComponentType: "llm", ComponentName: req.ComponentName, ResultText: req.Content.Full}
	default:
		return
	}
	g.simCtl.Record(ev, bi)
}

// jsonAnyOrString parses s as JSON so tool_args serializes as a structured object when possible,
// falling back to the raw string.
func jsonAnyOrString(s string) any {
	if s == "" {
		return nil
	}
	var v any
	if json.Unmarshal([]byte(s), &v) == nil {
		return v
	}
	return s
}
