// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Command tools is a flyedge-governed Anthropic tool-use agent. Claude may request a `fetch_url`
// tool; the agent runs guard.CheckToolCall (the tool_call stage) BEFORE executing it, so policy can
// allow or DENY the tool invocation — e.g. deny egress to an external destination. This governs the
// action the model wants to take, not just the prompt.
//
// The model call itself is governed by the transport wrap (pre_llm), and each tool call is gated
// explicitly in the loop. Env: COMPFLY_*/FLYEDGE_* + ANTHROPIC_API_KEY; PROMPT overrides the task.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	flyedge "github.com/compfly-ai/flyedge-go"
)

const session = "tools-demo"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	guard, err := flyedge.New(flyedge.LoadEnv())
	if err != nil {
		return err
	}
	defer guard.Close()
	fmt.Printf("flyedge guard: DID=%s\n", guard.DID())

	// pre_llm governed via the transport wrap; tool_call governed explicitly below.
	hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
	client := anthropic.NewClient(
		anthropicopt.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
		anthropicopt.WithHTTPClient(hc),
	)

	fetchTool := anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name:        "fetch_url",
		Description: anthropic.String("Fetch the contents of a URL over HTTP and return the body."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{"url": map[string]any{"type": "string", "description": "the URL to fetch"}},
			Required:   []string{"url"},
		},
	}}

	prompt := envOr("PROMPT", "Fetch https://example.com and tell me the HTTP status code you got.")
	fmt.Printf("task: %q\n", prompt)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	msgs := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))}

	for turn := 0; turn < 4; turn++ {
		msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.ModelClaudeHaiku4_5,
			MaxTokens: 512,
			Tools:     []anthropic.ToolUnionParam{fetchTool},
			Messages:  msgs,
		})
		if err != nil {
			if de, ok := flyedge.AsDenyError(err); ok {
				fmt.Printf("pre_llm BLOCKED: %s\n", de.Decision.Reason)
				break
			}
			return err
		}
		msgs = append(msgs, msg.ToParam())

		var toolResults []anthropic.ContentBlockParamUnion
		usedTool := false
		for _, block := range msg.Content {
			switch block.Type {
			case "text":
				fmt.Printf("claude: %s\n", block.Text)
			case "tool_use":
				usedTool = true
				tu := block.AsToolUse()
				toolResults = append(toolResults, runGuardedTool(ctx, guard, tu))
			}
		}
		if !usedTool {
			break // Claude produced a final answer
		}
		msgs = append(msgs, anthropic.NewUserMessage(toolResults...))
	}

	fmt.Println(guard.Report())
	return nil
}

// runGuardedTool gates a tool call through flyedge (tool_call stage) BEFORE executing it. On DENY
// it returns an error tool_result to the model instead of performing the action.
func runGuardedTool(ctx context.Context, guard *flyedge.Guard, tu anthropic.ToolUseBlock) anthropic.ContentBlockParamUnion {
	var args struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(tu.Input, &args)
	dest := ""
	if u, err := url.Parse(args.URL); err == nil {
		dest = u.Host
	}
	fmt.Printf("→ tool_call: %s(url=%s) dest=%s\n", tu.Name, args.URL, dest)

	// The gate: policy decides whether this tool invocation is allowed.
	if _, err := guard.CheckToolCall(ctx, session, tu.Name, string(tu.Input), dest); err != nil {
		if de, ok := flyedge.AsDenyError(err); ok {
			fmt.Printf("  DENIED by policy: %s — tool NOT executed\n", de.Decision.Reason)
			return anthropic.NewToolResultBlock(tu.ID, "blocked by security policy: "+de.Decision.Reason, true)
		}
		return anthropic.NewToolResultBlock(tu.ID, "policy check error: "+err.Error(), true)
	}

	// Allowed → actually perform the tool.
	fmt.Printf("  ALLOWED — executing\n")
	body, err := fetchURL(ctx, args.URL)
	if err != nil {
		return anthropic.NewToolResultBlock(tu.ID, "fetch error: "+err.Error(), true)
	}
	return anthropic.NewToolResultBlock(tu.ID, body, false)
}

func fetchURL(ctx context.Context, raw string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(b)), nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
