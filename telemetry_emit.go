package flyedge

import (
	"time"

	"github.com/compfly-ai/flyedge-go/telemetry"
)

// Explicit telemetry emit helpers. flyedge-go does not auto-instrument frameworks
// (the Python SDK's OpenInference path); instead the caller — or the flyedged daemon,
// which passively observes real model calls — hands the facts it already has to these
// helpers, which emit the event types prism recognizes (llm_io/tool_io/session_*).
// They route through the Guard's configured telemetry sink, so they only reach the
// platform when a cloud sink (WithCloudTelemetry) is installed; otherwise they're a
// no-op beyond the local recorder.

// RecordLLMCall emits an llm_io event — a model call's model/provider/token/latency
// facts — so cost/usage observability lands on the platform (prism records GenAI
// metrics for llm_io). Pass a streaming bool via RecordLLMCallStreamed when relevant.
func (g *Guard) RecordLLMCall(sessionID, requestID, model, provider string, inputTokens, outputTokens int64, latencyMS float64) {
	g.emitLLM(sessionID, requestID, model, provider, inputTokens, outputTokens, latencyMS, nil)
}

// RecordLLMCallStreamed is RecordLLMCall with an explicit streamed flag (stream_lifecycle).
func (g *Guard) RecordLLMCallStreamed(sessionID, requestID, model, provider string, inputTokens, outputTokens int64, latencyMS float64, streamed bool) {
	g.emitLLM(sessionID, requestID, model, provider, inputTokens, outputTokens, latencyMS, &streamed)
}

func (g *Guard) emitLLM(sessionID, requestID, model, provider string, inputTokens, outputTokens int64, latencyMS float64, streamed *bool) {
	if g == nil || g.tel == nil {
		return
	}
	g.tel.Record(telemetry.Event{
		Type: telemetry.EventLLMIO, SessionID: sessionID, RequestID: requestID,
		Model: model, Provider: provider, Operation: "chat",
		InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens,
		LatencyMS: latencyMS, Streaming: streamed, OccurredAt: time.Now(),
	})
}

// RecordToolIO emits a tool_io event (tool name + args/result). argsJSON/resultJSON are
// carried as the audit request/response payloads.
func (g *Guard) RecordToolIO(sessionID, requestID, toolName, argsJSON, resultJSON string) {
	if g == nil || g.tel == nil {
		return
	}
	g.tel.Record(telemetry.Event{
		Type: telemetry.EventToolIO, SessionID: sessionID, RequestID: requestID,
		Name: toolName, Operation: "tool.call",
		RequestFull: argsJSON, ResponseFull: resultJSON, OccurredAt: time.Now(),
	})
}

// RecordSessionStart / RecordSessionSummary emit agent-session lifecycle telemetry.
// data is an optional payload (e.g. rolled-up stats on summary).
func (g *Guard) RecordSessionStart(sessionID string, data map[string]any) {
	g.emitSession(telemetry.EventSessionStart, sessionID, data)
}

func (g *Guard) RecordSessionSummary(sessionID string, data map[string]any) {
	g.emitSession(telemetry.EventSessionSummary, sessionID, data)
}

func (g *Guard) emitSession(typ, sessionID string, data map[string]any) {
	if g == nil || g.tel == nil {
		return
	}
	g.tel.Record(telemetry.Event{Type: typ, SessionID: sessionID, Data: data, OccurredAt: time.Now()})
}
