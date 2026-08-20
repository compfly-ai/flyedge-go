// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Command otel is a flyedge-protected Claude agent whose protection events are exported as
// OpenTelemetry spans. It wires a stdout span exporter so you can WATCH each policy check land as a
// "flyedge.check" span (stage, action, model, latency) on the console — the same spans that would
// flow to your real collector / Datadog / Honeycomb in production.
//
// The point: the OTel sink (telemetry/otel) reports to YOUR observability stack, independent of the
// Compfly-native telemetry path. The application owns the TracerProvider + exporter + shutdown; the
// core flyedge library stays zero-dependency.
//
// Env:
//   COMPFLY_API_URL                 prism base (e.g. http://localhost:8080)
//   COMPFLY_AGENT_DID               the agent's DID (MCP-minted)
//   COMPFLY_AGENT_PRIVATE_KEY_PATH  Ed25519 PEM
//   ANTHROPIC_API_KEY               Claude key
//   PROMPT                          user prompt (default: a benign question)
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/compfly-ai/flyedge-go"
	feotel "github.com/compfly-ai/flyedge-go/telemetry/otel"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	// 1. Stand up an OTel pipeline WE own: a pretty stdout exporter behind a TracerProvider. In
	//    production this would be an OTLP exporter to your collector; the flyedge sink doesn't care.
	exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
	if err != nil {
		return fmt.Errorf("stdout exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	otel.SetTracerProvider(tp)
	defer func() {
		// Flush + release on shutdown — this is what prints the spans below the reply.
		_ = tp.Shutdown(ctx)
	}()

	// 2. Build the guard, installing the OTel sink as its telemetry (nil → the global tracer above).
	guard, err := flyedge.New(flyedge.LoadEnv(), flyedge.WithTelemetry(feotel.New(nil)))
	if err != nil {
		return fmt.Errorf("flyedge: %w", err)
	}
	defer guard.Close()
	fmt.Printf("flyedge guard: DID=%s (OTel telemetry → stdout)\n", guard.DID())

	// 3. One governed HTTP client into the Anthropic SDK — the pre_llm check emits a span per call.
	hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
	client := anthropic.NewClient(
		anthropicopt.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
		anthropicopt.WithHTTPClient(hc),
	)

	prompt := envOr("PROMPT", "What are your store hours?")
	fmt.Printf("prompt=%q\n", prompt)

	callCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	msg, err := client.Messages.New(callCtx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 256,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
	})
	if err != nil {
		if de, ok := flyedge.AsDenyError(err); ok {
			fmt.Printf("BLOCKED by policy: %s\n", de.Decision.Reason)
		} else {
			return err
		}
	} else {
		var out string
		for _, b := range msg.Content {
			if b.Type == "text" {
				out += b.Text
			}
		}
		fmt.Printf("reply: %s\n", out)
	}

	// Local aggregate still works alongside the span export.
	fmt.Println(guard.Report())
	fmt.Println("--- flyedge.check spans (exported on shutdown) ---")
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
