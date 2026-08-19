// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"context"
	"encoding/json"
)

// The transport wrap governs the pre_llm stage automatically. Tool calls and model responses
// happen in the caller's own loop, so these helpers make governing the tool_call and post_llm
// stages a one-liner — the explicit gate for tool execution and response inspection.

// CheckToolCall gates a tool invocation (the tool_call stage): run it before executing a tool so
// policy can allow/deny the call (e.g. deny egress to an external destination). args is serialized
// as the inspected content; destDomain is the tool's target (host/service) if it has one.
func (g *Guard) CheckToolCall(ctx context.Context, session, toolName string, args any, destDomain string) (Decision, error) {
	return g.Check(ctx, CheckRequest{
		SessionID:     session,
		Stage:         StageToolCall,
		ComponentType: "TOOL",
		ComponentName: toolName,
		MethodName:    "call",
		Content:       Content{Full: jsonString(args)},
		Operation:     Operation{Type: "tool.call", ToolName: toolName, DestDomain: destDomain, ToolArgsJSON: jsonString(args)},
	})
}

// CheckToolResponse gates a tool's output (the tool_call_response stage): run it on the result
// before feeding it back to the model, so policy can inspect returned content.
func (g *Guard) CheckToolResponse(ctx context.Context, session, toolName string, result any) (Decision, error) {
	return g.Check(ctx, CheckRequest{
		SessionID:     session,
		Stage:         StageToolCallResponse,
		ComponentType: "TOOL",
		ComponentName: toolName,
		MethodName:    "response",
		Content:       Content{Full: jsonString(result)},
		Operation:     Operation{Type: "tool.response", ToolName: toolName},
	})
}

// CheckModelResponse gates a model completion (the post_llm stage): run it on the model's output
// before returning it to the user, so policy can inspect generated content.
func (g *Guard) CheckModelResponse(ctx context.Context, session, model, text string) (Decision, error) {
	return g.Check(ctx, CheckRequest{
		SessionID:     session,
		Stage:         StagePostLLM,
		ComponentType: "LLM",
		ComponentName: model,
		MethodName:    "response",
		Content:       Content{Full: text},
		Operation:     Operation{Type: "chat.completions", ModelID: model},
	})
}

func jsonString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
