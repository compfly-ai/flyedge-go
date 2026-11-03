package flyedge

import (
	"fmt"
	"strings"
)

type historyIssue struct{ Code, Path, Message string }

// validateHistoryMessages checks a complete recorded conversation, never executing its tools.
func validateHistoryMessages(history []any) []historyIssue {
	issues := []historyIssue{}
	add := func(i int, message string) {
		issues = append(issues, historyIssue{"invalid_history", fmt.Sprintf("/setup/history/%d", i), message})
	}
	if len(history) > 128 {
		return []historyIssue{{"invalid_history", "/setup/history", "History supports at most 128 messages"}}
	}
	seen, pending := map[string]bool{}, map[string]bool{}
	for i, item := range history {
		m, ok := item.(map[string]any)
		if !ok {
			add(i, "History message must be an object")
			continue
		}
		role, _ := m["role"].(string)
		content, text := m["content"].(string)
		calls, hasCalls := m["toolCalls"]
		id, hasID := m["toolCallId"]
		for key := range m {
			if key != "role" && key != "content" && key != "toolCalls" && key != "toolCallId" {
				add(i, "Unknown history message field")
			}
		}
		if role == "tool" {
			callID, _ := id.(string)
			if hasCalls || !hasID || !pending[callID] || !text {
				add(i, "Tool response requires text content and an unresolved toolCallId")
				continue
			}
			delete(pending, callID)
			continue
		}
		if len(pending) > 0 {
			add(i, "Resolve every pending tool call before the next user or assistant message")
		}
		if hasID || (role != "user" && role != "assistant") {
			add(i, "Only user, assistant, and linked tool messages are supported")
			continue
		}
		if hasCalls {
			entries, ok := calls.([]any)
			if role != "assistant" || !ok || len(entries) == 0 {
				add(i, "Only assistant messages may contain nonempty toolCalls")
				continue
			}
			if _, exists := m["content"]; exists && !text {
				add(i, "Assistant content must be text when supplied")
			}
			for _, entry := range entries {
				call, ok := entry.(map[string]any)
				if !ok {
					add(i, "Tool call must be an object")
					continue
				}
				callID, _ := call["id"].(string)
				name, _ := call["name"].(string)
				_, args := call["arguments"].(map[string]any)
				if strings.TrimSpace(callID) == "" || strings.TrimSpace(name) == "" || !args || len(call) != 3 || seen[callID] {
					add(i, "Tool calls require a unique id, name, and arguments object")
					continue
				}
				seen[callID] = true
				pending[callID] = true
			}
		} else if !text || strings.TrimSpace(content) == "" {
			add(i, "User and assistant messages require text or assistant tool calls")
		}
	}
	if len(pending) > 0 {
		issues = append(issues, historyIssue{"invalid_history", "/setup/history", "History has unresolved tool calls"})
	}
	return issues
}
