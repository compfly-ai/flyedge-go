// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"
)

// oboHeader lets a caller select which seeded user a request acts on behalf of, so one served agent
// identity governs many principals. Absent → the server's default acting user.
const oboHeader = "X-CompFly-On-Behalf-Of"

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	SessionID string        `json:"session_id"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// serveHTTP exposes the governed agent over an OpenAI-compatible endpoint — the contract the
// CompFly playground and simulation/attack engines drive, so evaluations run against the REAL
// governed agent, not a stub. Runs until SIGINT, then shuts down gracefully (so the caller's
// defers — guard.Close, the protection report — still run).
func serveHTTP(ctx context.Context, a *agent, def *user, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/v1/chat/completions", chatHandler(a, def))

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server error: %v", err)
		}
	}()
	log.Printf("serving on %s (acting as %s) — POST /v1/chat/completions · %s header selects the principal",
		addr, def.Name, oboHeader)

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	<-sigCtx.Done()
	log.Println("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

// chatHandler runs one conversation turn through the governed agent. The acting principal is
// selected per request from the on-behalf-of header (falling back to the server's default), so one
// served agent identity governs many users: handle attaches that principal to the context, and
// every governed call in the turn carries the OBO envelope the gateway keys per-user policy on.
func chatHandler(a *agent, def *user) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"message": "invalid request body"}})
			return
		}
		u := def
		if obo := r.Header.Get(oboHeader); obo != "" {
			sel, ok := users[obo]
			if !ok {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{"message": "unknown principal: " + obo}})
				return
			}
			u = sel
		}
		session := req.SessionID
		if session == "" {
			session = "http-" + randHex()
		}
		reply, err := a.handle(r.Context(), u, session, lastUserMessage(req.Messages))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{"message": err.Error()}})
			return
		}
		mdl := req.Model
		if mdl == "" {
			mdl = a.provider.Model()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-" + randHex(),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   mdl,
			"choices": []map[string]any{{
				"index":         0,
				"message":       map[string]string{"role": "assistant", "content": reply.text},
				"finish_reason": "stop",
			}},
			"usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
		})
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
