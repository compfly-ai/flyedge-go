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
