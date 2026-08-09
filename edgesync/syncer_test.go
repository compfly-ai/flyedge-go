package edgesync_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compfly-ai/flyedge-go/edgesync"
)

// fakeTransport is a stub Transport — offline, deterministic, and lets tests swap the poll
// response mid-test to exercise change detection.
type fakeTransport struct {
	mu          sync.Mutex
	pollBody    []byte
	getCalls    int32
	postBodies  [][]byte
	postPaths   []string
	lastHeaders map[string]string
}

func (f *fakeTransport) GetSigned(_ context.Context, _ string, headers map[string]string) ([]byte, error) {
	atomic.AddInt32(&f.getCalls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastHeaders = headers
	return f.pollBody, nil
}

func (f *fakeTransport) PostSigned(_ context.Context, path string, body []byte) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.postPaths = append(f.postPaths, path)
	f.postBodies = append(f.postBodies, body)
	return nil, nil
}

func (f *fakeTransport) setPollBody(b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pollBody = b
}

func (f *fakeTransport) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.postPaths)
}

// A Poll call surfaces the raw bytes and reports change only against the PREVIOUS call this
// Syncer made — the core contract the loop is built on.
func TestPoll_ChangeDetection(t *testing.T) {
	ft := &fakeTransport{pollBody: []byte(`{"v":1}`)}
	s := edgesync.New(ft, "/v1/flyedge/edge-packs", time.Hour)

	raw, changed, err := s.Poll(context.Background())
	if err != nil || !changed || string(raw) != `{"v":1}` {
		t.Fatalf("first poll: raw=%s changed=%v err=%v (want changed=true)", raw, changed, err)
	}

	_, changed, err = s.Poll(context.Background())
	if err != nil || changed {
		t.Fatalf("second poll (same body): changed=%v err=%v (want changed=false)", changed, err)
	}

	ft.setPollBody([]byte(`{"v":2}`))
	_, changed, err = s.Poll(context.Background())
	if err != nil || !changed {
		t.Fatalf("third poll (body changed): changed=%v err=%v (want changed=true)", changed, err)
	}
}

// OnUpdate fires only on real content changes, never on a steady-state poll — the whole point of
// hashing rather than firing on every tick.
func TestStart_OnUpdateFiresOnlyOnChange(t *testing.T) {
	ft := &fakeTransport{pollBody: []byte(`{"v":1}`)}
	var updates int32
	var lastRaw []byte
	var mu sync.Mutex

	s := edgesync.New(ft, "/v1/flyedge/edge-packs", 10*time.Millisecond,
		edgesync.WithOnUpdate(func(raw []byte) {
			atomic.AddInt32(&updates, 1)
			mu.Lock()
			lastRaw = raw
			mu.Unlock()
		}),
	)
	s.Start()
	defer s.Stop()

	waitFor(t, func() bool { return atomic.LoadInt32(&updates) == 1 })
	mu.Lock()
	got := string(lastRaw)
	mu.Unlock()
	if got != `{"v":1}` {
		t.Fatalf("lastRaw = %q, want the initial poll body", got)
	}

	// Several more ticks with an UNCHANGED body must not fire OnUpdate again.
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&updates); n != 1 {
		t.Fatalf("updates = %d after steady-state ticks, want still 1", n)
	}

	// Changing the body must fire exactly one more update.
	ft.setPollBody([]byte(`{"v":2}`))
	waitFor(t, func() bool { return atomic.LoadInt32(&updates) == 2 })
}

// WithHeaders is attached to every poll GET — the seam a caller uses for hostname/manifest-hash
// style headers, mirroring config_poll.go's heartbeatHeaders.
func TestWithHeaders(t *testing.T) {
	ft := &fakeTransport{pollBody: []byte(`{}`)}
	s := edgesync.New(ft, "/p", time.Hour, edgesync.WithHeaders(func() map[string]string {
		return map[string]string{"X-Agent-Hostname": "laptop-1"}
	}))
	if _, _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := ft.lastHeaders["X-Agent-Hostname"]; got != "laptop-1" {
		t.Errorf("header not attached: got %q", got)
	}
}

// Report without a configured report path is a clear error, not a silent no-op — a caller that
// forgets WithReportPath should find out immediately, not ship nothing forever.
func TestReport_NoPathConfigured(t *testing.T) {
	ft := &fakeTransport{}
	s := edgesync.New(ft, "/p", time.Hour)
	if err := s.Report(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("expected an error reporting with no report path configured")
	}
	if ft.postCount() != 0 {
		t.Error("no POST should have been attempted")
	}
}

// Report signs and POSTs to exactly the configured path.
func TestReport_PostsToConfiguredPath(t *testing.T) {
	ft := &fakeTransport{}
	s := edgesync.New(ft, "/p", time.Hour, edgesync.WithReportPath("/p/report"))
	if err := s.Report(context.Background(), []byte(`{"installed":[]}`)); err != nil {
		t.Fatal(err)
	}
	if ft.postCount() != 1 || ft.postPaths[0] != "/p/report" {
		t.Fatalf("post paths = %v, want exactly one call to /p/report", ft.postPaths)
	}
}

// WithReportBuilder makes the loop auto-report on every tick, independent of whether the poll
// content changed — mirroring the existing "the pull doubles as the report" cadence.
func TestStart_AutoReportsEveryTick(t *testing.T) {
	ft := &fakeTransport{pollBody: []byte(`{}`)}
	s := edgesync.New(ft, "/p", 10*time.Millisecond,
		edgesync.WithReportPath("/p/report"),
		edgesync.WithReportBuilder(func() ([]byte, error) { return []byte(`{"installed":["x"]}`), nil }),
	)
	s.Start()
	defer s.Stop()

	waitFor(t, func() bool { return ft.postCount() >= 2 }) // immediate tick + at least one interval tick
	if ft.postPaths[0] != "/p/report" {
		t.Errorf("report path = %q", ft.postPaths[0])
	}
}

// Stop must be safe to call on a Syncer that was never Started, and must actually halt the loop
// (no more GETs after Stop returns).
func TestStop_NeverStartedIsSafe(t *testing.T) {
	s := edgesync.New(&fakeTransport{}, "/p", time.Hour)
	s.Stop() // must not panic or block
}

func TestStop_HaltsTheLoop(t *testing.T) {
	ft := &fakeTransport{pollBody: []byte(`{}`)}
	s := edgesync.New(ft, "/p", 5*time.Millisecond)
	s.Start()
	waitFor(t, func() bool { return atomic.LoadInt32(&ft.getCalls) >= 1 })
	s.Stop()
	n := atomic.LoadInt32(&ft.getCalls)
	time.Sleep(30 * time.Millisecond)
	if got := atomic.LoadInt32(&ft.getCalls); got != n {
		t.Errorf("GetSigned called %d more times after Stop", got-n)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
