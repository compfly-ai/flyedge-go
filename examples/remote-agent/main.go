// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CompFly AI
// remote-agent is a deterministic support agent with one read-only tool. It has
// no HTTP listener, provider credential, or billable model calls. It exercises
// real policy checks, simulation hooks and runtime telemetry through the SDK.
package main

import (
	"context"
	"encoding/json"
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
	environment := flag.String("environment", "dev", "agent environment label in Platform (deployment URL comes from COMPFLY_API_URL)")
	leaseDuration := flag.Duration("lease-duration", time.Hour, "renewable authorization window (1s–24h)")
	autoRenew := flag.Bool("auto-renew", true, "renew the lease until stopped or revoked; false makes this a finite session")
	logFormat := flag.String("log-format", "pretty", "log format: pretty, text, or json")
	color := flag.String("color", "auto", "terminal colors: auto, always, or never")
	flag.Parse()
	logger, err := newExampleLogger(*logFormat, *color)
	if err != nil {
		return err
	}
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
	// This dedicated simulation example explicitly enables these capabilities.
	// Ordinary SDK clients still opt in through InvocationOptions.
	c, err := g.ServeInvocations(ctx, flyedge.InvocationOptions{
		Name:                     "Remote support example",
		Environment:              *environment,
		Purposes:                 []flyedge.InvocationPurpose{flyedge.PurposePlayground, flyedge.PurposeSimulation, flyedge.PurposeEval},
		LeaseDuration:            *leaseDuration,
		AutoRenew:                autoRenew,
		AllowClientState:         true,
		AllowSimulationInjection: true,
		// Local Tilt uses HTTP. The SDK limits this exception to literal loopback addresses.
		AllowInsecureLoopback: true,
	}, handler(g, logger))
	if err != nil {
		return err
	}
	defer c.Close()
	logger.Info("connection.ready", "connection", c.ID, "lease_duration", *leaseDuration, "auto_renew", *autoRenew, "expires", c.ExpiresAt().Format(time.RFC3339))
	return c.Wait()
}
func handler(g *flyedge.Guard, baseLogger *slog.Logger) flyedge.InvocationHandler {
	return func(ctx context.Context, r flyedge.InvocationRequest, out flyedge.InvocationWriter) (invocationErr error) {
		logger := baseLogger.With(
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
		orderID, orderStatus, err := orderFromState(r.State)
		if err != nil {
			return err
		}
		if r.State != nil {
			logger.Info("invocation.state", "order_id", orderID)
		}
		stage = "tool_call"
		logger.Info("tool.requested", "tool", "lookup_order", "order_id", orderID)
		if _, err := g.CheckToolCall(ctx, r.SessionID, "lookup_order", map[string]any{"order_id": orderID}); err != nil {
			return err
		}
		logger.Info("policy.allowed", "stage", stage)
		logger.Info("tool.called", "tool", "lookup_order", "order_id", orderID)
		rawJSON, err := json.Marshal(map[string]string{"order_id": orderID, "status": orderStatus})
		if err != nil {
			return err
		}
		rawResult := string(rawJSON)
		logger.Info("tool.result", "tool", "lookup_order", "content", rawResult)
		stage = "tool_result"
		result, _, err := g.GovernToolResult(ctx, r.SessionID, "lookup_order", rawResult)
		if err != nil {
			return err
		}
		logger.Info("tool.governed_result", "tool", "lookup_order", "content", result)
		reply := fmt.Sprintf("Order %s is %s. Tool evidence: %s", orderID, orderStatus, result)
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
