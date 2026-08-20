// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Command docs-quickstart is the runnable form of the "Complete Example" in
// docs/DEVELOPER_GUIDE_GO.md. It is intentionally minimal — a single model turn
// with one governed tool (get_weather) — so the guide's headline snippet is real,
// compiling, runnable code rather than a placeholder.
//
// It governs the two stages that matter for a first integration:
//   - pre_llm:   guard.WrapRoundTripper installs ONE governed http.Client, so the
//                model call is checked before it leaves the process.
//   - tool_call: guard.CheckToolCall runs BEFORE the tool executes, so policy can
//                allow / warn / deny the action.
//
// Env:
//   COMPFLY_API_URL                 prism base (e.g. http://localhost:8080)
//   COMPFLY_AGENT_DID               the agent's DID (MCP-minted)
//   COMPFLY_AGENT_PRIVATE_KEY_PATH  Ed25519 PEM
//   ANTHROPIC_API_KEY               Claude key
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"

	"github.com/compfly-ai/flyedge-go"
)

const session = "docs-quickstart"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	// Guard from the environment; cloud telemetry every 10s.
	guard, err := flyedge.New(flyedge.LoadEnv(),
		flyedge.WithCloudTelemetry(10*time.Second),
	)
	if err != nil {
		return fmt.Errorf("flyedge init: %w", err)
	}
	defer guard.Close()

	ctx := context.Background()

	// Register the agent's manifest (framework, tools, models).
	if err := guard.Connect(ctx, flyedge.ManifestInfo{
		Framework:   "anthropic-sdk-go",
		Tools:       []string{"get_weather"},
		Models:      []string{"claude-sonnet-4-5"},
		Environment: "development",
	}); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	// One governed HTTP client for all model calls.
	hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
	client := anthropic.NewClient(
		anthropicopt.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
		anthropicopt.WithHTTPClient(hc),
	)

	sctx := flyedge.ContextWithSession(ctx, session)

	resp, err := client.Messages.New(sctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeSonnet4_5,
		MaxTokens: 1024,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("What's the weather in Paris?")),
		},
		Tools: toolDefs(),
	})
	if err != nil {
		if de, ok := flyedge.AsDenyError(err); ok {
			fmt.Println("model call blocked:", de.Decision.Reason)
			return nil
		}
		return err
	}

	// Guard each tool call before executing it.
	for _, block := range resp.Content {
		tu := block.AsToolUse()
		if tu.Name == "" {
			continue
		}
		if _, err := guard.CheckToolCall(sctx, session, tu.Name, string(tu.Input), "api.weather.com"); err != nil {
			if ke, ok := flyedge.AsKillSwitchError(err); ok {
				return fmt.Errorf("kill switch: %s", ke.Error())
			}
			if de, ok := flyedge.AsDenyError(err); ok {
				fmt.Printf("tool %q denied: %s\n", tu.Name, de.Decision.Reason)
				continue
			}
			return err
		}
		fmt.Printf("tool %q allowed → %s\n", tu.Name, executeTool(tu))
	}

	fmt.Println(guard.Report())
	return nil
}

// toolDefs declares the one tool this agent exposes.
func toolDefs() []anthropic.ToolUnionParam {
	return []anthropic.ToolUnionParam{
		{OfTool: &anthropic.ToolParam{
			Name:        "get_weather",
			Description: anthropic.String("Get the current weather for a city."),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{
					"city": map[string]any{"type": "string", "description": "the city name"},
				},
				Required: []string{"city"},
			},
		}},
	}
}

// executeTool runs an ALLOWED tool. Returns canned data so the quickstart needs no
// external egress.
func executeTool(tu anthropic.ToolUseBlock) string {
	switch tu.Name {
	case "get_weather":
		return "18°C, partly cloudy"
	default:
		return "unknown tool"
	}
}
