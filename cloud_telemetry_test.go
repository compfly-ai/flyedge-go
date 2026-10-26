package flyedge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloudTelemetryDefaultAndOptOut(t *testing.T) {
	enabled, disabled := true, false
	for _, tc := range []struct {
		name       string
		configured *bool
		opts       []Option
		deliveries int32
	}{
		{"default", nil, nil, 1},
		{"config disabled", &disabled, nil, 0},
		{"config enabled", &enabled, nil, 1},
		{"option enables", &disabled, []Option{WithCloudTelemetry(time.Hour)}, 1},
		{"option disables", &enabled, []Option{WithoutCloudTelemetry()}, 0},
		{"disabled", nil, []Option{WithoutCloudTelemetry()}, 0},
		{"reenabled", nil, []Option{WithoutCloudTelemetry(), WithCloudTelemetry(time.Hour)}, 1},
		{"custom", nil, []Option{WithTelemetry(&captureTelemetry{})}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deliveries atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/flyedge/telemetry" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				var batch struct {
					Events []struct {
						SessionID string `json:"session_id"`
						Type      string `json:"type"`
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
					t.Error(err)
				}
				if len(batch.Events) != 1 || batch.Events[0].SessionID != "telemetry-default" || batch.Events[0].Type != "tool_io" {
					t.Errorf("unexpected batch: %+v", batch)
				}
				deliveries.Add(1)
				w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			g, err := New(Config{APIURL: srv.URL, CloudTelemetry: tc.configured}, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			g.RecordToolIODetail(ToolIO{SessionID: "telemetry-default", ToolName: "lookup_order"})
			if err := g.Close(); err != nil {
				t.Fatal(err)
			}
			if got := deliveries.Load(); got != tc.deliveries {
				t.Fatalf("deliveries=%d, want %d", got, tc.deliveries)
			}
		})
	}
}
