package flyedge_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	flyedge "github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
)

// fakeGateway is a stub enforcer that ALSO implements the (unexported) signedPoster + signedGetter
// seams the poller uses — so Connect + the config heartbeat run fully offline. Go satisfies the
// unexported interfaces structurally, even from this external test package.
type fakeGateway struct {
	mu          sync.Mutex
	connectBody []byte
	configBody  []byte
	postPaths   []string
	getCalls    int32
	lastHeaders map[string]string
}

func (f *fakeGateway) Check(context.Context, enforce.CheckRequest) (enforce.Decision, error) {
	return enforce.Decision{Action: enforce.ActionAllow}, nil
}

func (f *fakeGateway) PostSigned(_ context.Context, path string, _ []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postPaths = append(f.postPaths, path)
	return f.connectBody, nil
}

func (f *fakeGateway) GetSigned(_ context.Context, _ string, headers map[string]string) ([]byte, error) {
	atomic.AddInt32(&f.getCalls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastHeaders = headers
	return f.configBody, nil
}

func (f *fakeGateway) connectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.postPaths {
		if p == "/v1/flyedge/connect" {
			n++
		}
	}
	return n
}

func (f *fakeGateway) headers() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastHeaders
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// The poller picks up model_mode from the ConnectResponse, then flips it (and detects a simulation)
// from a subsequent /config poll — firing the mode-change handler exactly once, not every tick.
func TestConfigPollModeChangeAndSimulation(t *testing.T) {
	var modeChanges int32
	var mu sync.Mutex
	var gotOld, gotNew flyedge.ModelMode

	fake := &fakeGateway{
		connectBody: []byte(`{"accepted":true,"heartbeat_interval_seconds":1,"model_mode":"check"}`),
		configBody: []byte(`{"model_mode":"passthrough","simulation":` +
			`{"active":true,"run_id":"run-1","middlewares":["telemetry","behavior_monitor"],` +
			`"telemetry_jwt":"jwt-x","telemetry_url":"ws://localhost:8080/v1/simulation/telemetry"}}`),
	}
	g, err := flyedge.New(flyedge.Config{}, flyedge.WithEnforcer(fake),
		flyedge.WithHeartbeat(10*time.Millisecond),
		flyedge.WithModeChangeHandler(func(o, n flyedge.ModelMode) {
			atomic.AddInt32(&modeChanges, 1)
			mu.Lock()
			gotOld, gotNew = o, n
			mu.Unlock()
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()

	if err := g.Connect(context.Background(), flyedge.ManifestInfo{Framework: "test"}); err != nil {
		t.Fatal(err)
	}

	waitFor(t, time.Second, func() bool {
		return g.SimulationActive() && g.ModelMode() == flyedge.ModelModePassthrough
	})

	// Let several more polls run; the mode-change handler must NOT re-fire on unchanged mode.
	time.Sleep(80 * time.Millisecond)
	if c := atomic.LoadInt32(&modeChanges); c != 1 {
		t.Fatalf("mode-change handler fired %d times, want exactly 1", c)
	}
	mu.Lock()
	if gotOld != flyedge.ModelModeCheck || gotNew != flyedge.ModelModePassthrough {
		t.Fatalf("mode change old=%q cur=%q, want check→passthrough", gotOld, gotNew)
	}
	mu.Unlock()

	sim := g.SimulationConfig()
	if sim == nil || !sim.Active || sim.RunID != "run-1" || sim.TelemetryJWT != "jwt-x" {
		t.Fatalf("simulation config = %+v", sim)
	}

	if h := fake.headers(); h["X-Agent-Heartbeat"] != "1" || h["X-Agent-Manifest-Hash"] == "" {
		t.Fatalf("heartbeat headers missing/incomplete: %v", h)
	}
}

// manifest_refresh_required fires the registered handler.
func TestConfigPollManifestRefreshHandler(t *testing.T) {
	var refreshes int32
	fake := &fakeGateway{
		connectBody: []byte(`{"accepted":true,"heartbeat_interval_seconds":1}`),
		configBody:  []byte(`{"manifest_refresh_required":true}`),
	}
	g, err := flyedge.New(flyedge.Config{}, flyedge.WithEnforcer(fake),
		flyedge.WithHeartbeat(10*time.Millisecond),
		flyedge.WithManifestRefreshHandler(func() { atomic.AddInt32(&refreshes, 1) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.Connect(context.Background(), flyedge.ManifestInfo{Framework: "test"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return atomic.LoadInt32(&refreshes) >= 1 })
}

// Without a handler, manifest_refresh_required defaults to re-sending the manifest (reconnect).
func TestConfigPollDefaultReconnect(t *testing.T) {
	fake := &fakeGateway{
		connectBody: []byte(`{"accepted":true,"heartbeat_interval_seconds":1}`),
		configBody:  []byte(`{"manifest_refresh_required":true}`),
	}
	g, err := flyedge.New(flyedge.Config{}, flyedge.WithEnforcer(fake), flyedge.WithHeartbeat(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.Connect(context.Background(), flyedge.ManifestInfo{Framework: "test"}); err != nil {
		t.Fatal(err)
	}
	// initial Connect POST + at least one reconnect triggered by the refresh flag.
	waitFor(t, time.Second, func() bool { return fake.connectCount() >= 2 })
}

// Default mode is check before any poll resolves it.
func TestModelModeDefaultsToCheck(t *testing.T) {
	g, err := flyedge.New(flyedge.Config{}, flyedge.WithEnforcer(&fakeGateway{}))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if g.ModelMode() != flyedge.ModelModeCheck {
		t.Fatalf("default ModelMode = %q, want check", g.ModelMode())
	}
	if g.SimulationActive() {
		t.Fatal("SimulationActive should be false with no poll")
	}
}
