// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package edgesync_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// conditionalTransport implements the optional conditionalGetter capability, answering 304 when
// the caller's If-None-Match matches what it would have sent.
type conditionalTransport struct {
	mu           sync.Mutex
	body         []byte
	etag         string
	lastIfNone   string
	notModifieds int32
	fullSends    int32
}

func (c *conditionalTransport) GetSigned(context.Context, string, map[string]string) ([]byte, error) {
	return nil, nil // unused — the conditional path takes precedence
}

func (c *conditionalTransport) PostSigned(context.Context, string, []byte) ([]byte, error) {
	return nil, nil
}

func (c *conditionalTransport) GetSignedConditional(_ context.Context, _ string, headers map[string]string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastIfNone = headers["If-None-Match"]
	if c.lastIfNone != "" && c.lastIfNone == c.etag {
		atomic.AddInt32(&c.notModifieds, 1)
		return nil, true, nil
	}
	atomic.AddInt32(&c.fullSends, 1)
	return c.body, false, nil
}

func (c *conditionalTransport) set(body []byte, etag string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = body
	c.etag = etag
}

// The steady state — the overwhelmingly common case — must transfer nothing: first poll gets the
// body, every later poll gets a 304 and reports no change.
func TestPoll_ConditionalGet_SteadyStateSends304(t *testing.T) {
	body := []byte(`{"packs":[{"slug":"a"}]}`)
	ct := &conditionalTransport{}
	ct.set(body, contentHashForTest(body))
	s := edgesync.New(ct, "/p", time.Hour)

	raw, changed, err := s.Poll(context.Background())
	if err != nil || !changed || string(raw) != string(body) {
		t.Fatalf("first poll: changed=%v err=%v raw=%s", changed, err, raw)
	}
	if got := atomic.LoadInt32(&ct.fullSends); got != 1 {
		t.Fatalf("expected 1 full send, got %d", got)
	}

	for i := 0; i < 3; i++ {
		raw, changed, err = s.Poll(context.Background())
		if err != nil || changed || raw != nil {
			t.Fatalf("steady-state poll %d: changed=%v err=%v raw=%v (want no change, no body)", i, changed, err, raw)
		}
	}
	if got := atomic.LoadInt32(&ct.notModifieds); got != 3 {
		t.Errorf("expected 3 not-modified responses, got %d", got)
	}
	if got := atomic.LoadInt32(&ct.fullSends); got != 1 {
		t.Errorf("expected still 1 full send after steady state, got %d", got)
	}
}

// When the content genuinely changes the ETag no longer matches, so the body comes back and the
// caller is told to act on it.
func TestPoll_ConditionalGet_ChangeBreaksTheMatch(t *testing.T) {
	v1 := []byte(`{"packs":[{"slug":"a"}]}`)
	v2 := []byte(`{"packs":[{"slug":"a"},{"slug":"b"}]}`)
	ct := &conditionalTransport{}
	ct.set(v1, contentHashForTest(v1))
	s := edgesync.New(ct, "/p", time.Hour)

	if _, changed, _ := s.Poll(context.Background()); !changed {
		t.Fatal("first poll should report change")
	}
	if _, changed, _ := s.Poll(context.Background()); changed {
		t.Fatal("second poll (unchanged) should not report change")
	}

	ct.set(v2, contentHashForTest(v2))
	raw, changed, err := s.Poll(context.Background())
	if err != nil || !changed || string(raw) != string(v2) {
		t.Fatalf("after change: changed=%v err=%v raw=%s", changed, err, raw)
	}
}

// The very first poll has nothing to compare against, so it must not send a stale/empty
// If-None-Match that a server could accidentally match.
func TestPoll_ConditionalGet_FirstPollSendsNoIfNoneMatch(t *testing.T) {
	body := []byte(`{}`)
	ct := &conditionalTransport{}
	ct.set(body, contentHashForTest(body))
	s := edgesync.New(ct, "/p", time.Hour)

	if _, _, err := s.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	ct.mu.Lock()
	got := ct.lastIfNone
	ct.mu.Unlock()
	if got != "" {
		t.Errorf("first poll sent If-None-Match %q, want empty", got)
	}
}

// A Transport WITHOUT the optional capability must keep working unchanged — same change
// detection, just a full body every time.
func TestPoll_FallsBackWhenTransportHasNoConditionalSupport(t *testing.T) {
	ft := &fakeTransport{pollBody: []byte(`{"v":1}`)}
	s := edgesync.New(ft, "/p", time.Hour)

	if _, changed, err := s.Poll(context.Background()); err != nil || !changed {
		t.Fatalf("first poll: changed=%v err=%v", changed, err)
	}
	if _, changed, err := s.Poll(context.Background()); err != nil || changed {
		t.Fatalf("second poll: changed=%v err=%v (want no change)", changed, err)
	}
	if atomic.LoadInt32(&ft.getCalls) != 2 {
		t.Errorf("expected 2 plain GETs, got %d", atomic.LoadInt32(&ft.getCalls))
	}
}

// contentHashForTest mirrors the package's internal content hash so a fake server can compute the
// same ETag the Syncer will compare against.
func contentHashForTest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
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