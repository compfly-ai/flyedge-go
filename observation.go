package flyedge

import (
	"github.com/compfly-ai/flyedge-go/telemetry"
	"time"
)

// UsageObservation identifies observed skill or hook use. It carries no policy
// verdict or content payload; Data describes the evidence, not enforcement.
type UsageObservation struct {
	Kind, Name                      string // Kind is "skill" or "hook".
	SessionID, RequestID            string
	EndpointID, InstanceKey, UserID string
	TraceID, SpanID, ParentSpanID   string
	AgentFramework                  string
	OccurredAt                      time.Time
	Data                            map[string]any
}

// RecordUsageObservation emits a skill_usage or hook_usage trace event.
// Unsupported kinds and observations without a name/session are discarded.
func (g *Guard) RecordUsageObservation(c UsageObservation) {
	if g == nil || g.tel == nil || (c.Kind != "skill" && c.Kind != "hook") || c.Name == "" || c.SessionID == "" {
		return
	}
	g.tel.Record(telemetry.Event{
		Type: c.Kind + "_usage", Operation: c.Kind + ".use", Name: c.Name,
		SessionID: c.SessionID, RequestID: c.RequestID,
		EndpointID: c.EndpointID, InstanceKey: c.InstanceKey, UserID: c.UserID,
		TraceID: c.TraceID, SpanID: c.SpanID, ParentSpanID: c.ParentSpanID,
		AgentFramework: c.AgentFramework, OccurredAt: orNow(c.OccurredAt), Data: c.Data,
	})
}

// TurnOutcome reports how an agent turn ended when the host names no single model or tool call:
// a user abort or an error that ended the turn. Data carries the outcome fields
// (execution_status, decided_by, outcome_stage, error_kind); Err is the host's error text.
type TurnOutcome struct {
	SessionID, RequestID            string
	EndpointID, InstanceKey, UserID string
	TraceID, SpanID, ParentSpanID   string
	AgentFramework                  string
	Err                             string
	OccurredAt                      time.Time
	Data                            map[string]any
}

// RecordTurnOutcome emits a turn_outcome event. Outcomes without a session or data are discarded.
func (g *Guard) RecordTurnOutcome(c TurnOutcome) {
	if g == nil || g.tel == nil || c.SessionID == "" || len(c.Data) == 0 {
		return
	}
	g.tel.Record(telemetry.Event{
		Type: telemetry.EventTurnOutcome, Operation: "turn.end",
		SessionID: c.SessionID, RequestID: c.RequestID,
		EndpointID: c.EndpointID, InstanceKey: c.InstanceKey, UserID: c.UserID,
		TraceID: c.TraceID, SpanID: c.SpanID, ParentSpanID: c.ParentSpanID,
		AgentFramework: c.AgentFramework, Err: c.Err, OccurredAt: orNow(c.OccurredAt), Data: c.Data,
	})
}
