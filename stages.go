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
	return g.CheckMCPToolCall(ctx, session, "", toolName, args, destDomain)
}

// CheckMCPToolCall gates a tool invocation served by an MCP server, naming the server separately
// from the tool. Callers that know the tool came from MCP should prefer it over CheckToolCall:
// mcpServer is what joins the call to a sanctioned/unsanctioned component, so a fused
// "server__tool" string in toolName alone leaves the server unqueryable. An empty mcpServer
// behaves exactly like CheckToolCall.
func (g *Guard) CheckMCPToolCall(ctx context.Context, session, mcpServer, toolName string, args any, destDomain string) (Decision, error) {
	return g.Check(ctx, CheckRequest{
		SessionID:     session,
		Stage:         StageToolCall,
		ComponentType: "TOOL",
		ComponentName: toolName,
		MethodName:    "call",
		Content:       Content{Full: jsonString(args)},
		Operation: Operation{
			Type:         "tool.call",
			ToolName:     toolName,
			DestDomain:   destDomain,
			ToolArgsJSON: jsonString(args),
			MCPServerID:  mcpServer,
		},
	})
}

// CheckToolResponse gates a tool's output (the tool_call_response stage): run it on the result
// before feeding it back to the model, so policy can inspect returned content.
func (g *Guard) CheckToolResponse(ctx context.Context, session, toolName string, result any) (Decision, error) {
	return g.CheckMCPToolResponse(ctx, session, "", toolName, result)
}

// CheckMCPToolResponse gates the output of an MCP-served tool, naming the server separately. The
// response stage carries the server too so a returning call joins to the same component as the
// outgoing one. An empty mcpServer behaves exactly like CheckToolResponse.
func (g *Guard) CheckMCPToolResponse(ctx context.Context, session, mcpServer, toolName string, result any) (Decision, error) {
	return g.Check(ctx, CheckRequest{
		SessionID:     session,
		Stage:         StageToolCallResponse,
		ComponentType: "TOOL",
		ComponentName: toolName,
		MethodName:    "response",
		Content:       Content{Full: jsonString(result)},
		Operation:     Operation{Type: "tool.response", ToolName: toolName, MCPServerID: mcpServer},
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
