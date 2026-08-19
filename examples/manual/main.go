// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Command manual shows the most explicit flyedge integration: the caller runs guard.Check itself,
// inline, right where the policy gate belongs — no transport wrap, no middleware. This is the
// "nothing hidden" path: the check is a visible line in the agent loop, and the LLM client is a
// plain, unmodified client. Use this when you want the gate obvious at the call site (backends,
// custom orchestration loops, non-HTTP model calls).
//
// Env: COMPFLY_API_URL, COMPFLY_AGENT_DID, COMPFLY_AGENT_PRIVATE_KEY_PATH, ANTHROPIC_API_KEY, PROMPT.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	flyedge "github.com/compfly-ai/flyedge-go"
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

	// A plain, unmodified Anthropic client — flyedge is NOT wired into it.
	client := anthropic.NewClient(anthropicopt.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))

	prompt := envOr("PROMPT", "What are your store hours?")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// ── The gate: one explicit call, right before the model call. ──
	if _, err := guard.Check(ctx, flyedge.CheckRequest{
		SessionID: "manual-demo",
		Stage:     flyedge.StagePreLLM,
		Content:   flyedge.Content{Full: prompt},
		Operation: flyedge.Operation{Type: "chat.completions", ModelID: "claude-haiku-4-5"},
	}); err != nil {
		if ke, ok := flyedge.AsKillSwitchError(err); ok {
			fmt.Printf("KILLED by operator: %s (kill switch always enforces, even fail-open)\n", ke.Error())
			return nil
		}
		if de, ok := flyedge.AsDenyError(err); ok {
			fmt.Printf("BLOCKED by policy: %s\n", de.Decision.Reason)
			return nil
		}
		return err // enforcement transport error under fail-closed
	}

	// Allowed → make the model call directly.
	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 256,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
	})
	if err != nil {
		return err
	}
	for _, b := range msg.Content {
		if b.Type == "text" {
			fmt.Printf("reply: %s\n", b.Text)
		}
	}
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
