// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Command agentframework runs a Microsoft Agent Framework Go agent through Flyedge.
//
// The governed HTTP transport covers OpenAI Chat Completions requests. The governedTool wrapper
// covers the framework's automatic function-tool calls: it checks tool input before execution and
// checks the result before the framework sends it back to the model.
//
// Env:
//
//	COMPFLY_API_URL                 CompFly gateway base URL (defaults to production)
//	COMPFLY_AGENT_DID               agent DID
//	COMPFLY_AGENT_PRIVATE_KEY_PATH  Ed25519 PEM signing key
//	FLYEDGE_MODE                    enforce|warn|audit|off (default warn)
//	OPENAI_API_KEY                  required for the model call
//	MODEL                           OpenAI Chat Completions model (default gpt-4o-mini)
//	PROMPT                          request for the agent
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/compfly-ai/flyedge-go"
	"github.com/microsoft/agent-framework-go/agent"
	"github.com/microsoft/agent-framework-go/provider/openaiprovider"
	"github.com/microsoft/agent-framework-go/tool"
	"github.com/microsoft/agent-framework-go/tool/functool"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

const sessionID = "agentframework-demo"

type weatherArgs struct {
	City string `json:"city"`
}

// governedTool makes a Microsoft Agent Framework function tool pass through Flyedge at both tool
// boundaries. Embedding keeps its schema and provider-facing metadata unchanged.
type governedTool struct {
	tool.FuncTool
	guard      *flyedge.Guard
	sessionID  string
	destDomain string
}

func (t governedTool) Call(ctx context.Context, args string) (any, error) {
	if _, err := t.guard.CheckToolCall(ctx, t.sessionID, t.Name(), args, t.destDomain); err != nil {
		return nil, err
	}

	result, err := t.FuncTool.Call(ctx, args)
	if err != nil {
		return nil, err
	}
	if _, err := t.guard.CheckToolResponse(ctx, t.sessionID, t.Name(), result); err != nil {
		return nil, err
	}
	return result, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	guard, err := flyedge.New(flyedge.LoadEnv())
	if err != nil {
		return fmt.Errorf("flyedge: %w", err)
	}
	defer guard.Close()

	ctx, cancel := context.WithTimeout(
		flyedge.ContextWithSession(context.Background(), sessionID),
		60*time.Second,
	)
	defer cancel()

	// Use the Chat Completions provider explicitly: Flyedge governs this endpoint through its
	// provider-aware RoundTripper. WithResponseCheck also checks non-streaming model output.
	hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport, flyedge.WithResponseCheck())}
	modelClient := openai.NewClient(
		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
		option.WithHTTPClient(hc),
	)

	weather := functool.MustNew(functool.Config{
		Name:        "get_weather",
		Description: "Get the weather for a city.",
	}, func(_ context.Context, input weatherArgs) (string, error) {
		return fmt.Sprintf("The weather in %s is sunny and 22°C.", input.City), nil
	})

	a := openaiprovider.NewChatCompletionsAgent(modelClient, openaiprovider.AgentConfig{
		Model:        envOr("MODEL", "gpt-4o-mini"),
		Instructions: "You are a concise weather assistant. Use get_weather for weather questions.",
		Config: agent.Config{
			Name: "CompFly weather assistant",
			Tools: []tool.Tool{governedTool{
				FuncTool:   weather,
				guard:      guard,
				sessionID:  sessionID,
				destDomain: "weather.example.com",
			}},
		},
	})

	resp, err := a.RunText(ctx, envOr("PROMPT", "What is the weather in Paris?")).Collect()
	if err != nil {
		if de, ok := flyedge.AsDenyError(err); ok {
			fmt.Printf("BLOCKED by policy: %s\n", de.Decision.Reason)
			return nil
		}
		if ke, ok := flyedge.AsKillSwitchError(err); ok {
			fmt.Printf("BLOCKED by kill switch: %s\n", ke.Error())
			return nil
		}
		return err
	}

	fmt.Println(resp.String())
	fmt.Println(guard.Report())
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
