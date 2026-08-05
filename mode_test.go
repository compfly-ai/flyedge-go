// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge_test

import (
	"context"
	"testing"

	flyedge "github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
)

// countingEnforcer records how many times Check was called, so a test can prove ModeOff never hits
// the decision point.
type countingEnforcer struct {
	dec   enforce.Decision
	err   error
	calls *int
}

func (c countingEnforcer) Check(context.Context, enforce.CheckRequest) (enforce.Decision, error) {
	*c.calls++
	return c.dec, c.err
}

// ModeOff short-circuits to allow WITHOUT calling the enforcer (local-dev posture).
func TestModeOffSkipsCheck(t *testing.T) {
	calls := 0
	// The enforcer would DENY if consulted — proving allow came from the short-circuit, not the stub.
	g := newGuard(t,
		flyedge.WithMode(flyedge.ModeOff),
		flyedge.WithEnforcer(countingEnforcer{dec: enforce.Decision{Action: flyedge.ActionDeny}, calls: &calls}),
	)
	dec, err := g.Check(context.Background(), flyedge.CheckRequest{Stage: flyedge.StageToolCall})
	if err != nil || dec.Action != flyedge.ActionAllow {
		t.Fatalf("mode off: want allow/nil, got dec=%+v err=%v", dec, err)
	}
	if calls != 0 {
		t.Fatalf("mode off must not call the enforcer; calls=%d", calls)
	}
}

// ModeEnforce upgrades an advisory server `warn` into a blocking deny (+ *DenyError).
func TestModeEnforceUpgradesWarnToDeny(t *testing.T) {
	g := newGuard(t,
		flyedge.WithMode(flyedge.ModeEnforce),
		flyedge.WithEnforcer(stubEnforcer{dec: enforce.Decision{Action: flyedge.ActionWarn, Reason: "risky_tool"}}),
	)
	dec, err := g.Check(context.Background(), flyedge.CheckRequest{Stage: flyedge.StageToolCall})
	if dec.Action != flyedge.ActionDeny {
		t.Fatalf("enforce: want warn upgraded to deny, got %+v", dec)
	}
	de, ok := flyedge.AsDenyError(err)
	if !ok {
		t.Fatalf("enforce: want *DenyError, got %v", err)
	}
	if de.Decision.Reason != "risky_tool" {
		t.Errorf("enforce: reason = %q, want risky_tool preserved", de.Decision.Reason)
	}
}

// In Warn/Audit an advisory `warn` stays advisory: returned as a Warn decision with no error.
func TestModeWarnAndAuditKeepWarnAdvisory(t *testing.T) {
	for _, m := range []flyedge.Mode{flyedge.ModeWarn, flyedge.ModeAudit} {
		g := newGuard(t,
			flyedge.WithMode(m),
			flyedge.WithEnforcer(stubEnforcer{dec: enforce.Decision{Action: flyedge.ActionWarn}}),
		)
		dec, err := g.Check(context.Background(), flyedge.CheckRequest{Stage: flyedge.StageToolCall})
		if err != nil || dec.Action != flyedge.ActionWarn {
			t.Fatalf("mode %s: want advisory warn/nil, got dec=%+v err=%v", m, dec, err)
		}
	}
}

// The invariant: a server deny ALWAYS enforces, even in the most permissive checking mode (audit).
func TestServerDenyEnforcesRegardlessOfMode(t *testing.T) {
	g := newGuard(t,
		flyedge.WithMode(flyedge.ModeAudit),
		flyedge.WithEnforcer(stubEnforcer{dec: enforce.Decision{Action: flyedge.ActionDeny, Reason: "blocked_tool"}}),
	)
	dec, err := g.Check(context.Background(), flyedge.CheckRequest{Stage: flyedge.StageToolCall})
	if dec.Action != flyedge.ActionDeny {
		t.Fatalf("audit: server deny must still block, got %+v", dec)
	}
	if _, ok := flyedge.AsDenyError(err); !ok {
		t.Fatalf("audit: server deny must return *DenyError, got %v", err)
	}
}
