package flyedge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/compfly-ai/flyedge-go/enforce"
	"github.com/compfly-ai/flyedge-go/localcontrol"
)

// countingEnforcer records whether the server was consulted, which is how these tests tell a local
// short-circuit apart from a server decision that happened to match.
type countingEnforcer struct {
	calls int
	dec   enforce.Decision
	err   error
}

func (c *countingEnforcer) Check(_ context.Context, _ enforce.CheckRequest) (enforce.Decision, error) {
	c.calls++
	return c.dec, c.err
}

func newTestGuard(t *testing.T, enf enforce.Enforcer, opts ...Option) *Guard {
	t.Helper()
	all := append([]Option{WithEnforcer(enf)}, opts...)
	g, err := New(Config{Mode: ModeEnforce}, all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

func sqlCall(query string) CheckRequest {
	return CheckRequest{
		SessionID:     "s1",
		Stage:         StageToolCall,
		ComponentType: "TOOL",
		ComponentName: "PostgresQueryTool",
		Content:       Content{Full: query},
	}
}

// The latency win and the offline story both rest on this: a local block must not consult the
// server at all.
func TestCheck_LocalBlockShortCircuitsTheServer(t *testing.T) {
	enf := &countingEnforcer{dec: enforce.Decision{Action: ActionAllow}}
	g := newTestGuard(t, enf, mustLocal(t, localcontrol.Config{Mode: localcontrol.ModeEnforce}))

	dec, err := g.Check(context.Background(), sqlCall("DROP TABLE users"))

	if enf.calls != 0 {
		t.Errorf("server was called %d times on a local block; want 0", enf.calls)
	}
	if dec.Action != ActionDeny {
		t.Errorf("action = %q, want deny", dec.Action)
	}
	var de *DenyError
	if !errors.As(err, &de) {
		t.Errorf("error = %v, want *DenyError", err)
	}
	if dec.Reason != "destructive_query" {
		t.Errorf("reason = %q, want destructive_query", dec.Reason)
	}
}

// Local evaluation may only ADD a no. Anything it allows still goes to the server, which stays
// authoritative — otherwise a local allow would be silently overriding platform policy.
func TestCheck_LocalAllowStillConsultsTheServer(t *testing.T) {
	enf := &countingEnforcer{dec: enforce.Decision{Action: ActionDeny, Reason: "server_says_no"}}
	g := newTestGuard(t, enf, mustLocal(t, localcontrol.Config{Mode: localcontrol.ModeEnforce}))

	dec, err := g.Check(context.Background(), sqlCall("SELECT 1"))

	if enf.calls != 1 {
		t.Fatalf("server called %d times; want 1", enf.calls)
	}
	if dec.Action != ActionDeny || dec.Reason != "server_says_no" {
		t.Errorf("got %q/%q, want the server's deny", dec.Action, dec.Reason)
	}
	var de *DenyError
	if !errors.As(err, &de) {
		t.Errorf("error = %v, want *DenyError", err)
	}
}

// A local finding that did not block still happened; dropping it would make a warn-mode rollout
// look like nothing was detected.
func TestCheck_LocalWarningRidesAlongWithTheServerDecision(t *testing.T) {
	enf := &countingEnforcer{dec: enforce.Decision{Action: ActionAllow}}
	g := newTestGuard(t, enf, mustLocal(t, localcontrol.Config{Mode: localcontrol.ModeWarn}))

	dec, err := g.Check(context.Background(), sqlCall("DROP TABLE users"))
	if err != nil {
		t.Fatalf("warn mode returned an error: %v", err)
	}
	if enf.calls != 1 {
		t.Errorf("server called %d times; want 1", enf.calls)
	}
	if dec.Action != ActionAllow {
		t.Errorf("action = %q, want allow (warn mode must not block)", dec.Action)
	}
	joined := strings.Join(dec.Warnings, ",")
	if !strings.Contains(joined, "database_safety") {
		t.Errorf("warnings %v lost the local finding", dec.Warnings)
	}
}

// ModeOff is checked before local evaluation, matching the documented contract that it is a purely
// local "do nothing" posture.
func TestCheck_GuardModeOffSkipsLocalControlsToo(t *testing.T) {
	enf := &countingEnforcer{}
	g, err := New(Config{Mode: ModeOff},
		WithEnforcer(enf), mustLocal(t, localcontrol.Config{Mode: localcontrol.ModeEnforce}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })

	dec, err := g.Check(context.Background(), sqlCall("DROP TABLE users"))
	if err != nil || dec.Action != ActionAllow || dec.Reason != "mode_off" {
		t.Errorf("got %q/%q/%v, want allow/mode_off/nil", dec.Action, dec.Reason, err)
	}
	if enf.calls != 0 {
		t.Errorf("server called %d times in mode off; want 0", enf.calls)
	}
}

// With no local config the Guard must behave exactly as before this feature existed.
func TestCheck_NoLocalControlsIsUnchangedBehavior(t *testing.T) {
	enf := &countingEnforcer{dec: enforce.Decision{Action: ActionAllow}}
	g := newTestGuard(t, enf)

	dec, err := g.Check(context.Background(), sqlCall("DROP TABLE users"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if enf.calls != 1 || dec.Action != ActionAllow {
		t.Errorf("calls=%d action=%q; want 1/allow", enf.calls, dec.Action)
	}
	if g.LocalControlDetectors() != nil {
		t.Errorf("detectors = %v, want nil when unconfigured", g.LocalControlDetectors())
	}
}

// A kill switch is not something local evaluation can pre-empt on an otherwise-clean call.
func TestCheck_ServerKillStillEnforcesWithLocalControlsOn(t *testing.T) {
	enf := &countingEnforcer{
		dec: enforce.Decision{Action: ActionAllow},
		err: &enforce.KilledError{Kill: enforce.KillInfo{Reason: "incident"}},
	}
	g := newTestGuard(t, enf, mustLocal(t, localcontrol.Config{Mode: localcontrol.ModeEnforce}))

	_, err := g.Check(context.Background(), sqlCall("SELECT 1"))
	if _, ok := AsKillSwitchError(err); !ok {
		t.Errorf("error = %v, want *KillSwitchError", err)
	}
}

// ── Runtime config swap ───────────────────────────────────────────────────────

func TestSetLocalControls_SwapsRules(t *testing.T) {
	enf := &countingEnforcer{dec: enforce.Decision{Action: ActionAllow}}
	g := newTestGuard(t, enf, mustLocal(t, localcontrol.Config{Mode: localcontrol.ModeEnforce}))

	// Not blocked by the built-ins.
	if _, err := g.Check(context.Background(), sqlCall("VACUUM FULL orders")); err != nil {
		t.Fatalf("baseline call was blocked: %v", err)
	}

	if err := g.SetLocalControls(localcontrol.Config{
		Mode:           localcontrol.ModeEnforce,
		DatabaseSafety: &localcontrol.DatabaseSafetyConfig{BlockedPatterns: []string{`\bVACUUM\s+FULL\b`}},
	}); err != nil {
		t.Fatalf("SetLocalControls: %v", err)
	}

	if _, err := g.Check(context.Background(), sqlCall("VACUUM FULL orders")); err == nil {
		t.Error("newly published rule did not take effect")
	}
}

// A rule set that will not compile is a publishing bug. Falling back to no protection would turn
// that bug into an outage of the guardrail, so the previous engine must survive.
func TestSetLocalControls_BadConfigKeepsThePreviousEngine(t *testing.T) {
	enf := &countingEnforcer{dec: enforce.Decision{Action: ActionAllow}}
	g := newTestGuard(t, enf, mustLocal(t, localcontrol.Config{Mode: localcontrol.ModeEnforce}))

	err := g.SetLocalControls(localcontrol.Config{
		Mode:           localcontrol.ModeEnforce,
		DatabaseSafety: &localcontrol.DatabaseSafetyConfig{BlockedPatterns: []string{`(unclosed`}},
	})
	if err == nil {
		t.Fatal("SetLocalControls accepted an uncompilable pattern")
	}

	// The previously-good rules are still enforcing.
	if _, err := g.Check(context.Background(), sqlCall("DROP TABLE users")); err == nil {
		t.Error("a rejected config disarmed the running engine")
	}
}

// ── Sync payload handling ─────────────────────────────────────────────────────

func TestApplyLocalControlPayload_AppliesAndReports(t *testing.T) {
	enf := &countingEnforcer{dec: enforce.Decision{Action: ActionAllow}}
	g := newTestGuard(t, enf)

	var gotErr error
	g.applyLocalControlPayload(
		[]byte(`{"version":7,"mode":"enforce"}`),
		func(_ localcontrol.Config, err error) { gotErr = err },
	)
	if gotErr != nil {
		t.Fatalf("apply hook saw an error: %v", gotErr)
	}

	if _, err := g.Check(context.Background(), sqlCall("DROP TABLE users")); err == nil {
		t.Error("published rules did not take effect")
	}

	raw, err := g.buildLocalControlReport()
	if err != nil {
		t.Fatalf("buildLocalControlReport: %v", err)
	}
	report := string(raw)
	for _, want := range []string{`"version":7`, `"mode":"enforce"`, "database_safety"} {
		if !strings.Contains(report, want) {
			t.Errorf("report %s missing %s", report, want)
		}
	}
}

// An agent stuck on old rules because the new ones will not parse is invisible server-side unless
// it says so.
func TestApplyLocalControlPayload_ReportsTheFailure(t *testing.T) {
	enf := &countingEnforcer{dec: enforce.Decision{Action: ActionAllow}}
	g := newTestGuard(t, enf)

	g.applyLocalControlPayload([]byte(`{"version":3,"mode":"enforce"}`), nil)
	g.applyLocalControlPayload([]byte(`{not json`), nil)

	raw, err := g.buildLocalControlReport()
	if err != nil {
		t.Fatalf("buildLocalControlReport: %v", err)
	}
	report := string(raw)
	if !strings.Contains(report, `"error"`) {
		t.Errorf("report %s did not surface the decode failure", report)
	}
	// The version still reflects what is actually running, not the payload that failed.
	if !strings.Contains(report, `"version":3`) {
		t.Errorf("report %s lost the applied version", report)
	}
}

func TestSyncLocalControls_RequiresASignedTransport(t *testing.T) {
	g := newTestGuard(t, &countingEnforcer{})
	if err := g.SyncLocalControls(); err == nil {
		t.Error("sync started on a transport that cannot sign")
	}
}

func TestTokensFromMetadata(t *testing.T) {
	for _, tc := range []struct {
		name string
		md   map[string]any
		want int
	}{
		{"int", map[string]any{MetadataTokens: 42}, 42},
		{"int64", map[string]any{MetadataTokens: int64(42)}, 42},
		{"float from json", map[string]any{MetadataTokens: float64(42)}, 42},
		{"absent", map[string]any{}, 0},
		{"nil map", nil, 0},
		{"wrong type", map[string]any{MetadataTokens: "42"}, 0},
	} {
		if got := tokensFromMetadata(tc.md); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

func mustLocal(t *testing.T, cfg localcontrol.Config) Option {
	t.Helper()
	return WithLocalControls(cfg)
}
