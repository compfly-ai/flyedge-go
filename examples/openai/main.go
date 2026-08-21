// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Command openai is a minimal flyedge-protected OpenAI agent. It builds a flyedge.Guard, installs
// ONE governed http.Client (guard.WrapRoundTripper) into the OpenAI SDK, and makes a governed chat
// completion — the pre_llm policy check runs against prism before the model call, and a policy Deny
// returns a *flyedge.DenyError so the provider is never contacted. It then demonstrates the
// tool_call stage explicitly with guard.CheckToolCall before a hypothetical tool runs.
//
// Env:
//   COMPFLY_API_URL               prism base (e.g. http://localhost:8080; defaults to prod)
//   COMPFLY_AGENT_DID             the agent's DID (MCP-minted; optional — checks fail open without it)
//   COMPFLY_AGENT_PRIVATE_KEY_PATH  Ed25519 PEM
//   FLYEDGE_MODE                  enforce|warn (default warn)
//   OPENAI_API_KEY                required for a real model call
//   MODEL                         OpenAI model id (default gpt-4o)
//   PROMPT                        the user prompt (default: a benign question)
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/compfly-ai/flyedge-go"
	"github.com/openai/openai-go"
	openaiopt "github.com/openai/openai-go/option"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	// 1. Build the guard from the environment (explicit; no globals, no import-time side effects).
	guard, err := flyedge.New(flyedge.LoadEnv())
	if err != nil {
		return fmt.Errorf("flyedge: %w", err)
	}
	defer guard.Close()
	fmt.Printf("flyedge guard: DID=%s mode=%s\n", guard.DID(), envOr("FLYEDGE_MODE", "warn"))

	// 2. ONE governed HTTP client, handed to the OpenAI SDK via WithHTTPClient.
	hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport, flyedge.WithResponseCheck())}
	client := openai.NewClient(
		openaiopt.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
		openaiopt.WithHTTPClient(hc),
	)

	prompt := envOr("PROMPT", "What are your store hours?")
	model := envOr("MODEL", string(openai.ChatModelGPT4o))
	fmt.Printf("provider=openai model=%s prompt=%q\n", model, prompt)

	ctx, cancel := context.WithTimeout(flyedge.ContextWithSession(context.Background(), "openai-demo"), 60*time.Second)
	defer cancel()

	// 3. Governed model call. A policy Deny surfaces as a typed *flyedge.DenyError.
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    model,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
	})
	if err != nil {
		if de, ok := flyedge.AsDenyError(err); ok {
			fmt.Printf("BLOCKED by policy: %s\n", de.Decision.Reason)
		} else {
			return err
		}
	} else if len(resp.Choices) > 0 {
		fmt.Printf("reply: %s\n", resp.Choices[0].Message.Content)
	}

	// 4. The tool_call stage is governed explicitly, before you execute a tool the model asked for.
	dec, err := guard.CheckToolCall(ctx, "openai-demo", "get_weather", map[string]any{"city": "Paris"}, "api.weather.com")
	if err != nil {
		if de, ok := flyedge.AsDenyError(err); ok {
			fmt.Printf("tool get_weather denied: %s\n", de.Decision.Reason)
		} else {
			return err
		}
	} else {
		fmt.Printf("tool get_weather: %s\n", dec.Action)
	}

	// Protection summary — printed explicitly by the caller (not auto-emitted).
	fmt.Println(guard.Report())
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
