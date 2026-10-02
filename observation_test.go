package flyedge

import (
	"github.com/compfly-ai/flyedge-go/telemetry"
	"testing"
	"time"
)

func TestUsageObservationCarriesIdentityAndTraceWithoutPolicy(t *testing.T) {
	for _, kind := range []string{"skill", "hook"} {
		t.Run(kind, func(t *testing.T) {
			sink := telemetry.NewRecorder()
			capture := &captureTelemetry{}
			g := &Guard{tel: capture}
			at := time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)
			g.RecordUsageObservation(UsageObservation{Kind: kind, Name: "team/workflow", SessionID: "session", RequestID: "call", TraceID: "trace", SpanID: "span", ParentSpanID: "parent", EndpointID: "endpoint", InstanceKey: "instance", UserID: "user", AgentFramework: "flyedged-hooks", OccurredAt: at, Data: map[string]any{"evidence": "explicit_invocation"}})
			if len(capture.events) != 1 {
				t.Fatalf("events: %+v", capture.events)
			}
			e := capture.events[0]
			if e.Type != kind+"_usage" || e.Operation != kind+".use" || e.Name != "team/workflow" || e.SessionID != "session" || e.TraceID != "trace" || e.SpanID != "span" || e.ParentSpanID != "parent" || e.EndpointID != "endpoint" || e.InstanceKey != "instance" || e.UserID != "user" || !e.OccurredAt.Equal(at) {
				t.Fatalf("observation lost identity: %+v", e)
			}
			if e.Action != "" || e.Stage != "" || e.RequestFull != "" || e.ResponseFull != "" {
				t.Fatalf("observation fabricated enforcement/content: %+v", e)
			}
			sink.Record(e)
			if sink.Report().Checks != 0 {
				t.Fatal("usage counted as policy check")
			}
		})
	}
}

func TestUsageObservationRejectsInvalidKindAndMissingName(t *testing.T) {
	capture := &captureTelemetry{}
	g := &Guard{tel: capture}
	for _, c := range []UsageObservation{{Kind: "tool", Name: "x"}, {Kind: "skill"}, {Kind: "hook", Name: "x"}} {
		g.RecordUsageObservation(c)
	}
	if len(capture.events) != 0 {
		t.Fatalf("invalid observations shipped: %+v", capture.events)
	}
	var nilGuard *Guard
	nilGuard.RecordUsageObservation(UsageObservation{Kind: "skill", Name: "x", SessionID: "s"})
}
