// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CompFly AI
// remote-agent is a deterministic support agent with one read-only tool. It has
// no HTTP listener, provider credential, or billable model calls. It exercises
// real policy checks, simulation hooks and runtime telemetry through the SDK.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/compfly-ai/flyedge-go"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
func run() error {
	local := flag.Bool("allow-loopback-http", false, "explicitly allow local HTTP gateway")
	injection := flag.Bool("allow-simulation-injection", false, "permit simulation mutation hooks on this dedicated agent")
	environment := flag.String("environment", "dev", "environment exposed to Platform")
	flag.Parse()
	cfg := flyedge.LoadEnv()
	cfg.FailMode = flyedge.FailClosed
	g, err := flyedge.New(cfg, flyedge.WithHeartbeat(time.Second), flyedge.WithCloudTelemetry(time.Second))
	if err != nil {
		return err
	}
	defer g.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err = g.Connect(ctx, flyedge.ManifestInfo{Framework: "flyedge-go/remote-agent", Environment: *environment, Tools: []string{"lookup_order"}, Models: []string{"deterministic-support"}}); err != nil {
		return err
	}
	c, err := g.ServeInvocations(ctx, flyedge.InvocationOptions{Name: "Remote support example", Environment: *environment, Purposes: []flyedge.InvocationPurpose{flyedge.PurposePlayground, flyedge.PurposeSimulation, flyedge.PurposeEval}, Lifetime: time.Hour, AllowInsecureLoopback: *local, AllowSimulationInjection: *injection}, handler(g))
	if err != nil {
		return err
	}
	defer c.Close()
	log.Printf("Remote invocation ready: connection=%s expires=%s", c.ID, c.ExpiresAt.Format(time.RFC3339))
	return c.Wait()
}
func handler(g *flyedge.Guard) flyedge.InvocationHandler {
	return func(ctx context.Context, r flyedge.InvocationRequest, out flyedge.InvocationWriter) (invocationErr error) {
		logger := slog.New(slog.NewTextHandler(os.Stderr, nil)).With(
			"invocation", r.ID, "purpose", r.Purpose, "run", r.RunID, "session", r.SessionID,
		)
		started := time.Now()
		stage := "input"
		logger.Info("invocation.started", "messages", len(r.Messages))
		defer func() {
			if invocationErr != nil {
				logger.Error("invocation.failed", "stage", stage, "error", invocationErr, "duration", time.Since(started))
			} else {
				logger.Info("invocation.completed", "duration", time.Since(started))
			}
		}()
		writeReply := func(reply string) error {
			stage = "output"
			if err := out.TextDelta(ctx, reply); err != nil {
				return err
			}
			logger.Info("invocation.output", "content", reply)
			return nil
		}
		prompt := ""
		for i, m := range r.Messages {
			logger.Info("invocation.input", "index", i, "role", m.Role, "content", m.Content)
			if m.Role == "user" {
				prompt = m.Content
			}
		}
		// The deterministic model records the actual input; it never interprets injected
		// text as executable code. Use a real model for model-resilience measurements.
		stage = "pre_llm"
		if _, err := g.Check(ctx, flyedge.CheckRequest{SessionID: r.SessionID, Stage: flyedge.StagePreLLM, ComponentType: "LLM", ComponentName: "deterministic-support", Content: flyedge.Content{Full: prompt}, Operation: flyedge.Operation{Type: "chat.completions", ModelID: "deterministic-support"}}); err != nil {
			var denied *flyedge.DenyError
			if errors.As(err, &denied) {
				logger.Warn("policy.denied", "stage", stage, "error", err)
				return writeReply("I cannot help with that request: policy denied it.")
			}
			return err
		}
		logger.Info("policy.allowed", "stage", stage)
		stage = "tool_call"
		logger.Info("tool.requested", "tool", "lookup_order", "order_id", "DEMO-42")
		if _, err := g.CheckToolCall(ctx, r.SessionID, "lookup_order", map[string]any{"order_id": "DEMO-42"}); err != nil {
			return err
		}
		logger.Info("policy.allowed", "stage", stage)
		logger.Info("tool.called", "tool", "lookup_order", "order_id", "DEMO-42")
		rawResult := `{"order_id":"DEMO-42","status":"shipped"}`
		logger.Info("tool.result", "tool", "lookup_order", "content", rawResult)
		stage = "tool_result"
		result, _, err := g.GovernToolResult(ctx, r.SessionID, "lookup_order", rawResult)
		if err != nil {
			return err
		}
		logger.Info("tool.governed_result", "tool", "lookup_order", "content", result)
		reply := fmt.Sprintf("Order DEMO-42 is shipped. Tool evidence: %s", result)
		if strings.Contains(strings.ToLower(prompt), "secret") || strings.Contains(strings.ToLower(prompt), "ignore") {
			reply = "I can help check an order, but cannot reveal secrets or ignore safety rules."
		}
		stage = "post_llm"
		if _, err := g.CheckModelResponse(ctx, r.SessionID, "deterministic-support", reply); err != nil {
			return err
		}
		logger.Info("policy.allowed", "stage", stage)
		return writeReply(reply)
	}
}
