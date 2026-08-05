// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Command langchaingo shows the SAME flyedge transport wrap governing a THIRD framework —
// langchaingo — with no framework-specific code. langchaingo's Anthropic LLM accepts a custom HTTP
// client (WithHTTPClient), so installing guard.WrapRoundTripper governs its model calls exactly as
// it does the raw Anthropic and OpenAI SDKs. This is the "range" check: one interception model,
// any HTTP LLM framework.
//
// Env: COMPFLY_*/FLYEDGE_* + ANTHROPIC_API_KEY; PROMPT overrides the question.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	flyedge "github.com/compfly-ai/flyedge-go"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/anthropic"
)

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
	fmt.Printf("flyedge guard: DID=%s (framework=langchaingo)\n", guard.DID())

	// One governed HTTP client — the same wrap as the raw SDKs — handed to langchaingo.
	hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
	llm, err := anthropic.New(
		anthropic.WithToken(os.Getenv("ANTHROPIC_API_KEY")),
		anthropic.WithModel("claude-haiku-4-5"),
		anthropic.WithHTTPClient(hc),
	)
	if err != nil {
		return err
	}

	prompt := envOr("PROMPT", "What are your store hours?")
	fmt.Printf("prompt: %q\n", prompt)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	reply, err := llms.GenerateFromSinglePrompt(ctx, llm, prompt)
	if err != nil {
		if de, ok := flyedge.AsDenyError(err); ok {
			fmt.Printf("BLOCKED by policy: %s\n", de.Decision.Reason)
		} else {
			return err
		}
	} else {
		fmt.Printf("reply: %s\n", reply)
	}
	fmt.Println(guard.Report())
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
