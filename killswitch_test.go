package flyedge_test

import (
	"context"
	"testing"

	flyedge "github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
)

// TestFullKillBlocksEvenFailOpen: a full-scope kill (enforce.KilledError, the 403 path) must always
// block — including under the default FailOpen, where a generic error would otherwise ALLOW. This
// is the correctness fix: a kill can never be fail-open'd through.
func TestFullKillBlocksEvenFailOpen(t *testing.T) {
	killed := &enforce.KilledError{Kill: enforce.KillInfo{KillID: "kill_1", Scope: "full", Reason: "compromised"}}
	// default guard = FailOpen
	g := newGuard(t, flyedge.WithEnforcer(errEnforcer{err: killed}))
	dec, err := g.Check(context.Background(), flyedge.CheckRequest{Stage: flyedge.StagePreLLM})
	if dec.Action != flyedge.ActionDeny {
		t.Fatalf("fail-open kill must DENY, got %+v", dec)
	}
	ke, ok := flyedge.AsKillSwitchError(err)
	if !ok {
		t.Fatalf("want *KillSwitchError, got %v", err)
	}
	if len(ke.Kills) != 1 || ke.Kills[0].Reason != "compromised" {
		t.Errorf("kill info = %+v", ke.Kills)
	}
	// and it must NOT be mistaken for a plain policy deny
	if _, isDeny := flyedge.AsDenyError(err); isDeny {
		t.Error("kill should be KillSwitchError, not DenyError")
	}
}

// TestNonFullKillFrom200: a model/tool kill returned in a 200 response's kills array also enforces.
func TestNonFullKillFrom200(t *testing.T) {
	dec := enforce.Decision{Action: flyedge.ActionAllow, Kills: []enforce.KillInfo{{KillID: "k2", Scope: "tool", Target: "fetch_url", Reason: "tool killed"}}}
	g := newGuard(t, flyedge.WithEnforcer(stubEnforcer{dec: dec}))
	out, err := g.Check(context.Background(), flyedge.CheckRequest{Stage: flyedge.StageToolCall})
	if out.Action != flyedge.ActionDeny {
		t.Fatalf("non-full kill must DENY, got %+v", out)
	}
	if _, ok := flyedge.AsKillSwitchError(err); !ok {
		t.Fatalf("want *KillSwitchError, got %v", err)
	}
}

type errEnforcer struct{ err error }

func (e errEnforcer) Check(context.Context, enforce.CheckRequest) (enforce.Decision, error) {
	return enforce.Decision{}, e.err
}
