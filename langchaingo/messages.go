// Package langchaingo converts reverse-invocation conversations into langchaingo messages.
//
// It lives in its own module so the core flyedge library stays dependency-free.
package langchaingo

import (
	"encoding/json"
	"fmt"

	flyedge "github.com/compfly-ai/flyedge-go"
	"github.com/tmc/langchaingo/llms"
)

// Messages returns supplied test history followed by the live conversation, for a stateless
// handler to pass to GenerateContent. Recorded tool calls are context; never execute them again.
func Messages(request flyedge.InvocationRequest) ([]llms.MessageContent, error) {
	conversation := request.Messages
	if request.History != nil {
		conversation = append(append([]flyedge.InvocationMessage{}, *request.History...), request.Messages...)
	}
	toolNames := map[string]string{}
	out := make([]llms.MessageContent, 0, len(conversation))
	for _, m := range conversation {
		switch {
		case m.Role == "user":
			out = append(out, llms.TextParts(llms.ChatMessageTypeHuman, m.Content))
		case m.Role == "assistant" && len(m.ToolCalls) == 0:
			out = append(out, llms.TextParts(llms.ChatMessageTypeAI, m.Content))
		case m.Role == "assistant":
			parts := []llms.ContentPart{}
			if m.Content != "" {
				parts = append(parts, llms.TextContent{Text: m.Content})
			}
			for _, call := range m.ToolCalls {
				args := call.Arguments
				if args == nil {
					args = map[string]any{}
				}
				encoded, err := json.Marshal(args)
				if err != nil {
					return nil, fmt.Errorf("tool call %s arguments: %w", call.ID, err)
				}
				toolNames[call.ID] = call.Name
				parts = append(parts, llms.ToolCall{ID: call.ID, Type: "function", FunctionCall: &llms.FunctionCall{Name: call.Name, Arguments: string(encoded)}})
			}
			out = append(out, llms.MessageContent{Role: llms.ChatMessageTypeAI, Parts: parts})
		case m.Role == "tool" && m.ToolCallID != "":
			out = append(out, llms.MessageContent{Role: llms.ChatMessageTypeTool, Parts: []llms.ContentPart{
				llms.ToolCallResponse{ToolCallID: m.ToolCallID, Name: toolNames[m.ToolCallID], Content: m.Content},
			}})
		default:
			// Invocation input is untrusted: system instructions stay owned by the application.
			return nil, fmt.Errorf("unsupported invocation message role: %s", m.Role)
		}
	}
	return out, nil
}
