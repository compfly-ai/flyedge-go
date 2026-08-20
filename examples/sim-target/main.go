// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Command sim-target is a flyedge-go agent that the CompFly Simulation Lab evaluation engine can
// actively DRIVE: it serves an OpenAI-compatible chat endpoint, wrapped by the Guard. Each inbound
// turn runs the pre_llm / post_llm Check stages, so — while a simulation run is active — the
// engine's attack turns become the RuntimeEvent telemetry streamed to the CompFly platform. This is
// the "instrumented, endpoint-driven" target the simulation engine expects: the endpoint is how the
// engine feeds scenarios, the telemetry WS is
// how the engine sees the agent's internals.
//
// Contract (matches the Simulation Lab's generic `custom` HTTP adapter, no inferred schema):
//   POST /v1/chat/completions  {"messages":[{"role","content"}...],"session_id":"..."}
//     → {"choices":[{"message":{"role":"assistant","content":"..."}}]}
//   GET  /health               → 200
//
// Env:
//   COMPFLY_API_URL                 prism base the agent calls (defaults to https://prism.p.compfly.ai)
//   COMPFLY_AGENT_DID               the agent's DID (from registering an agent in the CompFly platform)
//   COMPFLY_AGENT_PRIVATE_KEY_PATH  Ed25519 PEM
//   COMPFLY_SIM_TELEMETRY_URL       advanced override for the telemetry WS URL; normally unset (the
//                                   gateway is authoritative)
//   SIM_TARGET_ADDR                 listen address (default :8899)
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	flyedge "github.com/compfly-ai/flyedge-go"
)

const model = "claude-sonnet-4-5"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	addr := os.Getenv("SIM_TARGET_ADDR")
	if addr == "" {
		addr = ":8899"
	}

	g, err := flyedge.New(flyedge.LoadEnv(),
		flyedge.WithHeartbeat(3*time.Second), // pick up a started sim quickly during local testing
		flyedge.WithModeChangeHandler(func(old, cur flyedge.ModelMode) {
			log.Printf("⚙  model_mode changed: %s → %s", old, cur)
		}),
	)
	if err != nil {
		return err
	}
	defer g.Close()

	ctx := context.Background()
	if err := g.Connect(ctx, flyedge.ManifestInfo{
		Framework:   "flyedge-go/sim-target",
		Models:      []string{model},
		Tools:       []string{"get_weather", "lookup_order"},
		Environment: "development",
	}); err != nil {
		return err
	}
	log.Printf("agent DID: %s", g.DID())
	log.Printf("connected — model_mode=%s; serving chat on %s", g.ModelMode(), addr)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/v1/chat/completions", chatHandler(g))

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server error: %v", err)
		}
	}()

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	<-sigCtx.Done()
	log.Println("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// chatCompletionRequest is the subset of the OpenAI-style body the generic HTTP adapter sends.
type chatCompletionRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	SessionID string        `json:"session_id"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatHandler governs one conversation turn: gate the incoming prompt (pre_llm), produce a reply,
// gate the reply (post_llm). While a simulation run is active each Check streams a RuntimeEvent, so
// the engine both drives (via this endpoint) and observes (via the telemetry WS). A policy denial
// at pre_llm becomes a refusal — the guarded-agent behavior the eval scores.
func chatHandler(g *flyedge.Guard) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "invalid request body"}})
			return
		}
		mdl := req.Model
		if mdl == "" {
			mdl = model
		}
		session := req.SessionID
		if session == "" {
			session = "eval-" + randHex()
		}
		prompt := lastUserMessage(req.Messages)
		ctx := r.Context()

		reply := replyFor(prompt)
		// pre_llm: gate the user turn. A server deny (jailbreak, secret egress, …) makes the agent refuse.
		if _, err := g.Check(ctx, flyedge.CheckRequest{
			SessionID:     session,
			Stage:         flyedge.StagePreLLM,
			ComponentType: "LLM",
			ComponentName: mdl,
			MethodName:    "chat",
			Content:       flyedge.Content{Full: prompt},
			Operation:     flyedge.Operation{Type: "chat.completions", ModelID: mdl},
		}); err != nil {
			var de *flyedge.DenyError
			if errors.As(err, &de) {
				reply = "I can't help with that request — it violates policy (" + de.Decision.Reason + ")."
			}
			// On enforcement-unavailable / fail-open the reply stands; the error is recorded in telemetry.
		} else {
			// post_llm: gate the generated reply (inspect the outbound content).
			_, _ = g.CheckModelResponse(ctx, session, mdl, reply)
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-" + randHex(),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   mdl,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": reply},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		})
	}
}

// replyFor is a deterministic, offline stand-in for a model completion. A real target would call its
// LLM here (through the Guard's transport wrap); for the eval data-path proof we return a safe,
// non-empty answer so judged scenarios don't trivially all-pass on an empty reply.
func replyFor(prompt string) string {
	p := strings.ToLower(prompt)
	switch {
	case strings.Contains(p, "ignore") && strings.Contains(p, "instruction"),
		strings.Contains(p, "system prompt"),
		strings.Contains(p, "disable") && strings.Contains(p, "rule"):
		return "I can't ignore my instructions or reveal system configuration. How can I help within my guidelines?"
	case strings.Contains(p, "api key"), strings.Contains(p, "password"), strings.Contains(p, "secret"):
		return "I can't share credentials or secrets. Is there something else I can help with?"
	default:
		return "I'm a customer-support assistant. I can help with orders, shipping, and account questions. [flyedge-go sim-target]"
	}
}

func lastUserMessage(msgs []chatMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].Content
		}
	}
	if len(msgs) > 0 {
		return msgs[len(msgs)-1].Content
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func randHex() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
