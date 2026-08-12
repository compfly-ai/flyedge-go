package flyedge

import (
	"testing"

	"github.com/compfly-ai/flyedge-go/telemetry"
)

type captureTelemetry struct {
	events []telemetry.Event
}

func (c *captureTelemetry) Record(ev telemetry.Event) {
	c.events = append(c.events, ev)
}

func (c *captureTelemetry) Report() telemetry.Summary {
	return telemetry.Summary{ByStage: map[string]int{}}
}

func (c *captureTelemetry) Close() error {
	return nil
}

func TestRecordToolIODetailCarriesEndpointAttribution(t *testing.T) {
	tel := &captureTelemetry{}
	g := &Guard{tel: tel}

	g.RecordToolIODetail(ToolIO{
		SessionID: "sess-1", RequestID: "tool-1", ToolName: "Bash",
		EndpointID: "endpoint-1", InstanceKey: "claude-code:/repo",
		AgentFramework: "flyedged-hooks",
	})

	if len(tel.events) != 1 {
		t.Fatalf("expected 1 telemetry event, got %d", len(tel.events))
	}
	got := tel.events[0]
	if got.Type != telemetry.EventToolIO || got.Name != "Bash" || got.Operation != "tool.call" {
		t.Fatalf("unexpected event shape: %+v", got)
	}
	if got.EndpointID != "endpoint-1" || got.InstanceKey != "claude-code:/repo" {
		t.Fatalf("endpoint attribution not carried: %+v", got)
	}
	if got.AgentFramework != "flyedged-hooks" {
		t.Fatalf("agent framework not carried: %+v", got)
	}
}
