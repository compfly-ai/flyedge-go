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

// LLMCall carries the facts of one model call. Token counts are the provider's own, reported as
// the provider reports them: InputTokens is the UNCACHED count, with cache reads/writes separate.
// The wire sums them into input_tokens and ships the breakdown alongside — callers never have to
// decide how to combine the tiers.
type LLMCall struct {
	SessionID string
	RequestID string
	Model     string
	Provider  string

	InputTokens      int64 // uncached input, as the provider reports it
	OutputTokens     int64
	CacheReadTokens  int64 // prompt-cache hits; frequently orders of magnitude above InputTokens
	CacheWriteTokens int64 // prompt-cache creation

	LatencyMS float64
	Streamed  *bool
}

// RecordLLMCallDetail emits an llm_io event from a full LLMCall, including cache tiers. Prefer it
// over RecordLLMCall for any provider that reports prompt caching: without the cache counts the
// platform's view of input volume is wrong by orders of magnitude, not by a rounding error.
func (g *Guard) RecordLLMCallDetail(c LLMCall) {
	if g == nil || g.tel == nil {
		return
	}
	g.tel.Record(telemetry.Event{
		Type: telemetry.EventLLMIO, SessionID: c.SessionID, RequestID: c.RequestID,
		Model: c.Model, Provider: c.Provider, Operation: "chat",
		InputTokens: c.InputTokens, OutputTokens: c.OutputTokens,
		TotalTokens:      c.InputTokens + c.OutputTokens,
		CacheReadTokens:  c.CacheReadTokens,
		CacheWriteTokens: c.CacheWriteTokens,
		LatencyMS:        c.LatencyMS, Streaming: c.Streamed, OccurredAt: time.Now(),
	})
}

func (g *Guard) emitLLM(sessionID, requestID, model, provider string, inputTokens, outputTokens int64, latencyMS float64, streamed *bool) {
	g.RecordLLMCallDetail(LLMCall{
		SessionID: sessionID, RequestID: requestID, Model: model, Provider: provider,
		InputTokens: inputTokens, OutputTokens: outputTokens, LatencyMS: latencyMS, Streamed: streamed,
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
