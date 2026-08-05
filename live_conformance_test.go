// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge_test

import (
	"context"
	"os"
	"testing"
	"time"

	flyedge "github.com/compfly-ai/flyedge-go"
)

// TestLiveConformance drives every path added in the spec-conformance pass (#91-94) against a
// running prism, proving prism ACCEPTS the enriched wire and RECORDS the new signals:
//   - trace/span correlation (a known trace id we can then find in ClickHouse)
//   - enrichment context (framework/provider/origin_type/execution_context/auth_context)
//   - rich telemetry vocabulary (llm_io/tool_io/session_start/session_summary)
//   - session taint read
//
// Env-gated exactly like TestLiveCheck:
//
//	FLYEDGE_LIVE=1 COMPFLY_API_URL=http://localhost:8080 \
//	COMPFLY_AGENT_DID=... COMPFLY_AGENT_PRIVATE_KEY_PATH=... \
//	FLYEDGE_TRACE_ID=<32hex> go test -run TestLiveConformance -v
func TestLiveConformance(t *testing.T) {
	if os.Getenv("FLYEDGE_LIVE") == "" {
		t.Skip("set FLYEDGE_LIVE=1 (+ COMPFLY_* + running stack)")
	}
	cfg := flyedge.LoadEnv()
	cfg.Timeout = 15 * time.Second
	g, err := flyedge.New(cfg, flyedge.WithMode(flyedge.ModeEnforce), flyedge.WithCloudTelemetry(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Close()
	t.Logf("guard DID=%s", g.DID())

	traceID := os.Getenv("FLYEDGE_TRACE_ID")
	if traceID == "" {
		traceID = "aabbccddeeff00112233445566778899"
	}
	spanID := "1122334455667788"

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	// Pin an explicit W3C trace so the recorded check is findable in ClickHouse by trace_id.
	ctx = flyedge.ContextWithTrace(ctx, traceID, spanID)

	sessionID := "go-conformance"
	lastAuth := int64(3)
	// A fully-enriched check: every field added in #93 is populated so prism must accept them all.
	enriched := flyedge.CheckRequest{
		RequestID:  "go-conformance-enriched",
		SessionID:  sessionID,
		Stage:      flyedge.StagePreLLM,
		Framework:  "flyedge-go-conformance",
		Layer:      "framework",
		Provider:   "openai",
		OriginType: flyedge.OriginTypeUser,
		Content:    flyedge.Content{Preview: "what are your hours?", Full: "what are your hours?"},
		Operation:  flyedge.Operation{Type: "chat.completions", ModelID: "gpt-4o", DestDomain: "api.openai.com"},
		ExecutionContext: &flyedge.ExecutionContext{
			Environment: "dev", IsAutonomous: false, TriggerType: "user_message",
		},
		AuthContext: &flyedge.AuthContext{
			Method: "oauth", UserGroups: []string{"staff"}, Department: "support",
			ClearanceLevel: "standard", LastAuthMinutes: &lastAuth,
		},
	}
	dec, err := g.Check(ctx, enriched)
	if dec.Reason == "fail_open" {
		t.Fatalf("enriched check failed (prism rejected the enriched body or signature): %s", dec.Message)
	}
	if err != nil {
		if _, ok := flyedge.AsDenyError(err); !ok {
			t.Fatalf("unexpected error on enriched check: %v", err)
		}
	}
	t.Logf("ENRICHED check accepted → action=%s reason=%s trace_id=%s", dec.Action, dec.Reason, traceID)

	// Rich telemetry vocabulary (#91): these ship as distinct activity types, not protection events.
	g.RecordSessionStart(sessionID, map[string]any{"framework": "flyedge-go-conformance"})
	g.RecordLLMCall(sessionID, "go-conformance-llm", "gpt-4o", "openai", 42, 17, 812.5)
	g.RecordToolIO(sessionID, "go-conformance-tool", "get_hours", `{"location":"HQ"}`, `{"hours":"9-5"}`)
	g.RecordSessionSummary(sessionID, map[string]any{"turns": 1})
	t.Log("emitted llm_io + tool_io + session_start + session_summary telemetry")

	// Session taint read (#94): untainted session returns (nil, nil), not an error.
	taint, err := g.SessionTaint(ctx, sessionID)
	if err != nil {
		t.Fatalf("SessionTaint returned an error (should be nil for untainted): %v", err)
	}
	if taint == nil {
		t.Log("SessionTaint → untainted (nil), as expected")
	} else {
		t.Logf("SessionTaint → severity=%.2f taints=%d", taint.SessionSeverity, len(taint.Taints))
	}

	if err := g.Close(); err != nil {
		t.Fatalf("Close/telemetry flush rejected by prism: %v", err)
	}
	t.Log("TELEMETRY batch (rich vocabulary) flushed + accepted by prism")
}
