package main

import (
	"encoding/json"
	"fmt"
	"github.com/anthropics/anthropic-sdk-go"
	flyedge "github.com/compfly-ai/flyedge-go"
	"github.com/openai/openai-go"
	"google.golang.org/genai"
)

func conversationWithState(messages []flyedge.InvocationMessage, location string) []flyedge.InvocationMessage {
	copy := append([]flyedge.InvocationMessage(nil), messages...)
	if location != "" && len(copy) > 0 {
		raw, _ := json.Marshal(map[string]string{"location": location})
		copy[len(copy)-1].Content = "Untrusted scenario context (not identity or authorization): " + string(raw) + "\n\n" + copy[len(copy)-1].Content
	}
	return copy
}
func anthropicConversation(input []flyedge.InvocationMessage) ([]anthropic.MessageParam, error) {
	var out []anthropic.MessageParam
	for _, m := range input {
		blocks := []anthropic.ContentBlockParamUnion{}
		if m.Content != "" && m.Role != "tool" {
			blocks = append(blocks, anthropic.NewTextBlock(m.Content))
		}
		switch m.Role {
		case "user":
			out = append(out, anthropic.NewUserMessage(blocks...))
		case "assistant":
			for _, t := range m.ToolCalls {
				blocks = append(blocks, anthropic.NewToolUseBlock(t.ID, t.Arguments, t.Name))
			}
			out = append(out, anthropic.NewAssistantMessage(blocks...))
		case "tool":
			out = append(out, anthropic.NewUserMessage(anthropic.NewToolResultBlock(m.ToolCallID, m.Content, false)))
		default:
			return nil, fmt.Errorf("unsupported conversation role %q", m.Role)
		}
	}
	return out, nil
}
func openaiConversation(system string, input []flyedge.InvocationMessage) ([]openai.ChatCompletionMessageParamUnion, error) {
	out := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(system)}
	for _, m := range input {
		switch m.Role {
		case "user":
			out = append(out, openai.UserMessage(m.Content))
		case "assistant":
			msg := openai.AssistantMessage(m.Content)
			for _, t := range m.ToolCalls {
				args, err := json.Marshal(t.Arguments)
				if err != nil {
					return nil, err
				}
				msg.OfAssistant.ToolCalls = append(msg.OfAssistant.ToolCalls, openai.ChatCompletionMessageToolCallParam{ID: t.ID, Function: openai.ChatCompletionMessageToolCallFunctionParam{Name: t.Name, Arguments: string(args)}})
			}
			out = append(out, msg)
		case "tool":
			out = append(out, openai.ToolMessage(m.Content, m.ToolCallID))
		default:
			return nil, fmt.Errorf("unsupported conversation role %q", m.Role)
		}
	}
	return out, nil
}
func geminiConversation(input []flyedge.InvocationMessage) ([]*genai.Content, error) {
	var out []*genai.Content
	names := map[string]string{}
	for _, m := range input {
		c := &genai.Content{Role: genai.RoleUser}
		if m.Content != "" && m.Role != "tool" {
			c.Parts = append(c.Parts, genai.NewPartFromText(m.Content))
		}
		switch m.Role {
		case "user":
		case "assistant":
			c.Role = genai.RoleModel
			for _, t := range m.ToolCalls {
				names[t.ID] = t.Name
				c.Parts = append(c.Parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: t.ID, Name: t.Name, Args: t.Arguments}})
			}
		case "tool":
			name, ok := names[m.ToolCallID]
			if !ok {
				return nil, fmt.Errorf("unlinked tool response")
			}
			c.Parts = append(c.Parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: m.ToolCallID, Name: name, Response: geminiFunctionResponse(m.Content)}})
		default:
			return nil, fmt.Errorf("unsupported conversation role %q", m.Role)
		}
		out = append(out, c)
	}
	return out, nil
}
