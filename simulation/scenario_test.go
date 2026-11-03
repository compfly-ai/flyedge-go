package simulation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func fixture(enabled bool) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"id":"p","session_id":"eval:run:s","step_id":"ask","overrides":[{"id":"r","enabled":%t,"tool":"read","occurrence":1,"arguments":{"/grade":"A"},"value":{"amount":7},"required_matches":1}]}`, enabled))
}
func TestScenarioMockExecutionSeparation(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		var m scenarioMocks
		m.configure(fixture(enabled))
		args := map[string]any{"grade": "A"}
		m.observe("other", "component.executed", "read", map[string]any{})
		m.observe("eval:run:s", "component.requested", "read", map[string]any{"arguments": args})
		_, mocked, err := m.before("eval:run:s", "read", args, true)
		if err != nil || mocked != enabled {
			t.Fatalf("mocked=%v err=%v", mocked, err)
		}
		if !mocked {
			m.observe("eval:run:s", "component.executed", "read", map[string]any{"result": 2})
		}
		args["grade"] = "changed"
		if len(m.events) != 2 || m.events[0]["arguments"].(map[string]any)["grade"] != "A" || m.status()["complete"] != true {
			t.Fatalf("bad evidence: %+v", m.status())
		}
		want := "component.executed"
		if enabled {
			want = "component.mocked"
		}
		if m.events[1]["event"] != want {
			t.Fatal(m.events)
		}
	}
}
func TestScenarioValidationAndMatching(t *testing.T) {
	for _, raw := range []string{`{}`, strings.Replace(string(fixture(true)), `"occurrence":1`, `"occurrence":0`, 1), strings.Replace(string(fixture(true)), `"required_matches":1`, `"required_matches":2`, 1), strings.Replace(string(fixture(true)), `"value":{"amount":7},`, "", 1), strings.Replace(string(fixture(true)), `"step_id":"ask",`, `"unknown":1,`, 1), strings.Repeat("x", 256*1024+1)} {
		var m scenarioMocks
		m.configure(json.RawMessage(raw))
		if _, _, err := m.before("eval:run:s", "read", nil, true); err == nil {
			t.Fatalf("accepted %s", raw[:min(len(raw), 80)])
		}
	}
	var m scenarioMocks
	m.configure(fixture(true))
	for _, test := range []struct {
		session string
		allowed bool
	}{{"wrong", true}, {"eval:run:s", false}} {
		if _, _, err := m.before(test.session, "read", map[string]any{"grade": "A"}, test.allowed); err == nil {
			t.Fatal("scope/auth accepted")
		}
	}
	if _, ok, err := m.before("eval:run:s", "other", nil, true); ok || err != nil {
		t.Fatal("unrelated tool was blocked")
	}
	if _, ok, err := m.before("eval:run:s", "read", map[string]any{"grade": "A"}, true); !ok || err != nil {
		t.Fatal(err)
	}
	m.configure(fixture(true)) // Repeated config must not reset occurrences/matches.
	if _, _, err := m.before("eval:run:s", "read", map[string]any{"grade": "A"}, true); err == nil {
		t.Fatal("retry escaped isolation")
	}
	if m.status()["complete"] != false {
		t.Fatal("failed plan complete")
	}
	m.configure(json.RawMessage(strings.Replace(string(fixture(true)), `"id":"p"`, `"id":"p2"`, 1)))
	if _, _, err := m.before("eval:run:s", "read", map[string]any{"grade": "B"}, true); err == nil {
		t.Fatal("argument mismatch executed")
	}
}
func TestScenarioConflictAndNull(t *testing.T) {
	var m scenarioMocks
	raw := strings.Replace(string(fixture(true)), `"value":{"amount":7}`, `"value":null`, 1)
	m.configure(json.RawMessage(raw))
	v, ok, err := m.before("eval:run:s", "read", map[string]any{"grade": "A"}, true)
	if err != nil || !ok || string(v) != "null" {
		t.Fatal("null mock lost", string(v), ok, err)
	}
	var p map[string]any
	_ = json.Unmarshal(fixture(true), &p)
	first := p["overrides"].([]any)[0].(map[string]any)
	second := map[string]any{}
	for k, v := range first {
		second[k] = v
	}
	second["id"] = "r2"
	p["overrides"] = []any{first, second}
	b, _ := json.Marshal(p)
	m.configure(b)
	if _, _, err := m.before("eval:run:s", "read", map[string]any{"grade": "A"}, true); err == nil {
		t.Fatal("conflict accepted")
	}
}
func TestScenarioEvidenceLimits(t *testing.T) {
	var m scenarioMocks
	m.configure(fixture(false))
	for i := 0; i <= maxScenarioEvents; i++ {
		m.observe("eval:run:s", "component.executed", "read", map[string]any{})
	}
	if len(m.events) != maxScenarioEvents || m.status()["complete"] != false {
		t.Fatal("overflow not failed")
	}
	m.configure(fixture(true))
	m.observe("eval:run:s", "component.executed", "read", map[string]any{"result": strings.Repeat("x", maxScenarioBytes)})
	if m.err == "" || len(m.events) != 0 {
		t.Fatal("byte overflow not failed")
	}
}
func TestScenarioPointer(t *testing.T) {
	v := map[string]any{"a/b": map[string]any{"~": []any{"yes"}}}
	if got, ok := jsonPointer(v, "/a~1b/~0/0"); !ok || got != "yes" {
		t.Fatal(got, ok)
	}
	for _, path := range []string{"/a~1b/~0/00", "/a~1b/~0/-1", "/a~2b"} {
		if _, ok := jsonPointer(v, path); ok {
			t.Fatal(path)
		}
	}
}
func TestScenarioSnapshotsRenewalAndCompletion(t *testing.T) {
	type received struct {
		auth  string
		event map[string]any
	}
	messages := make(chan received, 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			_, b, err := ws.Read(context.Background())
			if err != nil {
				return
			}
			var ev map[string]any
			if json.Unmarshal(b, &ev) == nil && ev["event_type"] == "scenario.snapshot" {
				messages <- received{r.Header.Get("Authorization"), ev}
			}
		}
	}))
	defer srv.Close()
	c := New("test")
	defer c.Stop()
	extra := append([]byte(`{"scenario_overrides":`), fixture(true)...)
	extra = append(extra, '}')
	cfg := Config{Active: true, RunID: "run", TelemetryJWT: "first", TelemetryURL: "ws" + strings.TrimPrefix(srv.URL, "http"), Extra: extra}
	c.OnConfigChange(&cfg)
	next := func() received {
		t.Helper()
		select {
		case ev := <-messages:
			return ev
		case <-time.After(3 * time.Second):
			t.Fatal("missing snapshot")
			return received{}
		}
	}
	ready := next()
	if ready.event["scenario_phase"] != "ready" || ready.event["prompt_id"] != "s" || ready.auth != "Bearer first" {
		t.Fatal(ready)
	}
	token := c.ScenarioToken()
	c.ObserveScenarioTool("eval:run:s", "component.requested", "read", map[string]any{})
	if _, _, err := c.BeforeScenarioTool("eval:run:s", "read", map[string]any{"grade": "A"}, true); err != nil {
		t.Fatal(err)
	}
	renewed := cfg
	renewed.TelemetryJWT = "second"
	c.OnConfigChange(&renewed)
	ev := next()
	snap := ev.event["scenario_snapshot"].(map[string]any)
	if ev.auth != "Bearer second" || len(snap["events"].([]any)) != 2 || snap["complete"] != true {
		t.Fatal(ev)
	}
	c.FinishScenario("run", "eval:run:s", token, false)
	ev = next()
	if ev.event["scenario_phase"] != "complete" || ev.event["scenario_success"] != false {
		t.Fatal(ev)
	}
	// Concurrent observations/status/renewals are safe and do not erase the plan.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 10; n++ {
				c.ScenarioStatus()
				c.ObserveScenarioTool("other", "component.executed", "read", map[string]any{})
			}
		}()
	}
	wg.Wait()
	c.FinishScenario("other", "eval:run:s", token, true)
	c.FinishScenario("run", "other", token, true)
	changed := renewed
	changed.Extra = json.RawMessage(strings.Replace(string(extra), `"step_id":"ask"`, `"step_id":"changed"`, 1))
	c.OnConfigChange(&changed)
	next()
	c.FinishScenario("run", "eval:run:s", token, true)
	select {
	case ev := <-messages:
		t.Fatalf("stale completion: %v", ev)
	case <-time.After(30 * time.Millisecond):
	}
}
func TestScenarioSnapshotQueueOverflow(t *testing.T) {
	c := New("test")
	c.state = StateActive
	c.runID = "run"
	c.tr = newTransport("", "")
	c.mocks.configure(fixture(false))
	for i := 0; i < wsQueueSize; i++ {
		c.tr.queue <- "occupied"
	}
	c.scenarioSnapshotLocked("complete", true)
	if c.mocks.status()["complete"] != false {
		t.Fatal("lost snapshot not marked inconclusive")
	}
}

func TestExecutionPlansArePrivate(t *testing.T) {
	c := New("test")
	plan := func(session, value string) json.RawMessage {
		return json.RawMessage(`{"id":"p","session_id":"` + session + `","step_id":"s","overrides":[{"id":"r","tool":"read","occurrence":1,"arguments":{},"value":"` + value + `","required_matches":1}]}`)
	}
	for _, s := range []string{"a", "b"} {
		if err := c.BeginExecution(s, plan(s, s)); err != nil {
			t.Fatal(err)
		}
	}
	if c.BeginExecution("a", plan("a", "bad")) == nil {
		t.Fatal("duplicate accepted")
	}
	for _, s := range []string{"b", "a"} {
		value, mocked, err := c.BeforeScenarioTool(s, "read", map[string]any{}, true)
		if err != nil || !mocked || string(value) != `"`+s+`"` {
			t.Fatalf("%s: %s %v %v", s, value, mocked, err)
		}
		if snapshot := c.EndExecution(s); snapshot["complete"] != true {
			t.Fatal(snapshot)
		}
	}
	if c.EndExecution("a") != nil {
		t.Fatal("execution retained")
	}
	if c.BeginExecution("wrong", plan("a", "a")) == nil {
		t.Fatal("scope mismatch accepted")
	}
}
