// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Command gemini is a minimal flyedge-protected Gemini agent. Google's genai client accepts an
// *http.Client via ClientConfig.HTTPClient, so the SAME guard.WrapRoundTripper wrap that governs the
// Anthropic and OpenAI SDKs governs Gemini too — no provider-specific adapter. The pre_llm policy
// check runs against prism before the model call; a policy Deny returns a *flyedge.DenyError. It then
// demonstrates the tool_call stage explicitly with guard.CheckToolCall.
//
// Env:
//   COMPFLY_API_URL               prism base (e.g. http://localhost:8080; defaults to prod)
//   COMPFLY_AGENT_DID             the agent's DID (MCP-minted; optional — checks fail open without it)
//   COMPFLY_AGENT_PRIVATE_KEY_PATH  Ed25519 PEM
//   FLYEDGE_MODE                  enforce|warn (default warn)
//   GEMINI_API_KEY                required for a real model call
//   MODEL                         Gemini model id (default gemini-3.6-flash)
//   PROMPT                        the user prompt (default: a benign question)
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/compfly-ai/flyedge-go"
	"google.golang.org/genai"
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

	prompt := envOr("PROMPT", "What are your store hours?")
	model := envOr("MODEL", "gemini-3.6-flash")
	fmt.Printf("provider=gemini model=%s prompt=%q\n", model, prompt)

	ctx, cancel := context.WithTimeout(flyedge.ContextWithSession(context.Background(), "gemini-demo"), 60*time.Second)
	defer cancel()

	// 2. ONE governed HTTP client, handed to the genai client via ClientConfig.HTTPClient.
	hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport, flyedge.WithResponseCheck())}
	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:     os.Getenv("GEMINI_API_KEY"),
		HTTPClient: hc,
		Backend:    genai.BackendGeminiAPI,
	})
	if err != nil {
		return fmt.Errorf("gemini client: %w", err)
	}

	// 3. Governed model call. A policy Deny surfaces as a typed *flyedge.DenyError.
	contents := []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)}
	resp, err := client.Models.GenerateContent(ctx, model, contents, nil)
	if err != nil {
		if de, ok := flyedge.AsDenyError(err); ok {
			fmt.Printf("BLOCKED by policy: %s\n", de.Decision.Reason)
		} else {
			return err
		}
	} else {
		fmt.Printf("reply: %s\n", resp.Text())
	}

	// 4. The tool_call stage is governed explicitly, before you execute a tool the model asked for.
	dec, err := guard.CheckToolCall(ctx, "gemini-demo", "get_weather", map[string]any{"city": "Paris"}, "api.weather.com")
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
