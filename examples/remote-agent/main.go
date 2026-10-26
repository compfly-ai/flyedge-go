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
	"github.com/compfly-ai/flyedge-go"
	"log"
	"os"
	"os/signal"
	"strings"
	"time"
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
	return func(ctx context.Context, r flyedge.InvocationRequest, out flyedge.InvocationWriter) error {
		prompt := ""
		for _, m := range r.Messages {
			if m.Role == "user" {
				prompt = m.Content
			}
		}
		// The deterministic model records the actual input; it never interprets injected
		// text as executable code. Use a real model for model-resilience measurements.
		if _, err := g.Check(ctx, flyedge.CheckRequest{SessionID: r.SessionID, Stage: flyedge.StagePreLLM, ComponentType: "LLM", ComponentName: "deterministic-support", Content: flyedge.Content{Full: prompt}, Operation: flyedge.Operation{Type: "chat.completions", ModelID: "deterministic-support"}}); err != nil {
			var denied *flyedge.DenyError
			if errors.As(err, &denied) {
				return out.TextDelta(ctx, "I cannot help with that request: policy denied it.")
			}
			return err
		}
		if _, err := g.CheckToolCall(ctx, r.SessionID, "lookup_order", map[string]any{"order_id": "DEMO-42"}); err != nil {
			return err
		}
		result, _, err := g.GovernToolResult(ctx, r.SessionID, "lookup_order", `{"order_id":"DEMO-42","status":"shipped"}`)
		if err != nil {
			return err
		}
		reply := fmt.Sprintf("Order DEMO-42 is shipped. Tool evidence: %s", result)
		if strings.Contains(strings.ToLower(prompt), "secret") || strings.Contains(strings.ToLower(prompt), "ignore") {
			reply = "I can help check an order, but cannot reveal secrets or ignore safety rules."
		}
		if _, err := g.CheckModelResponse(ctx, r.SessionID, "deterministic-support", reply); err != nil {
			return err
		}
		log.Printf("invocation=%s run=%s session=%s completed", r.ID, r.RunID, r.SessionID)
		return out.TextDelta(ctx, reply)
	}
}
