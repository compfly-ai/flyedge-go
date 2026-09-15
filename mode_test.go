// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge_test

import (
	"context"
	"testing"

	"github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
)

// countingEnforcer records how many times Check was called so the test can prove
// SDK-local mode never bypasses the platform decision point.
type countingEnforcer struct {
	dec   enforce.Decision
	err   error
	calls *int
}

func (c countingEnforcer) Check(context.Context, enforce.CheckRequest) (enforce.Decision, error) {
	*c.calls++
	return c.dec, c.err
}

func TestModeOffStillAppliesPlatformDeny(t *testing.T) {
	calls := 0
	g := newGuard(t,
		flyedge.WithMode(flyedge.ModeOff),
		flyedge.WithEnforcer(countingEnforcer{dec: enforce.Decision{Action: flyedge.ActionDeny}, calls: &calls}),
	)
	dec, err := g.Check(context.Background(), flyedge.CheckRequest{Stage: flyedge.StageToolCall})
	if _, denied := flyedge.AsDenyError(err); !denied || dec.Action != flyedge.ActionDeny {
		t.Fatalf("mode off: platform deny must block, got dec=%+v err=%v", dec, err)
	}
	if calls != 1 {
		t.Fatalf("mode off must call the enforcer once; calls=%d", calls)
	}
}

// In Warn/Audit an advisory `warn` stays advisory: returned as a Warn decision with no error.
func TestModeWarnAndAuditKeepWarnAdvisory(t *testing.T) {
	for _, m := range []flyedge.Mode{flyedge.ModeOff, flyedge.ModeWarn, flyedge.ModeAudit, flyedge.ModeEnforce} {
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
