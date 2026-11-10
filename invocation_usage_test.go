package flyedge

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func measured(n uint64) *uint64 { return &n }

func usageContext(g *Guard, invocation, session string) (context.Context, *invocationUsage) {
	u := newInvocationUsage(g, invocation, session)
	return context.WithValue(context.Background(), invocationUsageContextKey{}, u), u
}

func TestInvocationUsageMeasuredPresenceAndDeduplication(t *testing.T) {
	g := &Guard{}
	ctx, usage := usageContext(g, "turn", "session")
	counts := &ModelUsage{InputTokens: measured(10), CachedInputTokens: measured(3), OutputTokens: measured(2), ReasoningTokens: measured(1)}
	g.RecordInvocationUsage(ctx, "first", "model-a", counts)
	g.RecordInvocationUsage(ctx, "first", "model-a", counts)
	g.RecordInvocationUsage(ctx, "second", "model-b", &ModelUsage{InputTokens: measured(0), OutputTokens: measured(0)})
	*counts.InputTokens = 999 // The caller no longer owns the recorded counters.
	got := usage.finish()
	if got == nil || got.Version != 1 || got.InvocationID != "turn" || got.SessionID != "session" || got.ModelCalls != 2 || got.ReportedCalls != 2 || got.UnknownCalls != 0 {
		t.Fatalf("invalid measured envelope: %+v", got)
	}
	if *got.InputTokens != 10 || *got.OutputTokens != 2 || *got.TotalTokens != 12 || *got.CachedInputTokens != 3 || *got.ReasoningTokens != 1 {
		t.Fatalf("incorrect measured counts: %+v", got.ModelUsage)
	}
	if len(got.ByModel) != 2 || got.ByModel[0].Model != "model-a" || *got.ByModel[1].TotalTokens != 0 {
		t.Fatalf("model rows lost deterministic order or measured zero: %+v", got.ByModel)
	}
	if usage.finish() != nil {
		t.Fatal("completion was not consumed exactly once")
	}
}

func TestInvocationUsageUnknownAndCancelledCallsKeepTotalsUnknown(t *testing.T) {
	for _, name := range []string{"unknown", "partial", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			g := &Guard{}
			ctx, usage := usageContext(g, "turn", "session")
			var unknown *ModelUsage
			if name != "unknown" {
				unknown = &ModelUsage{InputTokens: measured(7), CachedInputTokens: measured(3)}
			}
			if name == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
				unknown.OutputTokens, unknown.ReasoningTokens, unknown.TotalTokens = measured(2), measured(1), measured(9)
			}
			g.RecordInvocationUsage(ctx, "unknown", "model", unknown)
			g.RecordInvocationUsage(context.WithoutCancel(ctx), "known", "model", &ModelUsage{InputTokens: measured(3), OutputTokens: measured(4)})
			got := usage.finish()
			if got == nil || got.ModelCalls != 2 || got.ReportedCalls != 1 || got.UnknownCalls != 1 || got.TotalTokens != nil || got.ByModel[0].TotalTokens != nil {
				t.Fatalf("unknown attempt disappeared behind measured call: %+v", got)
			}
			if *got.OutputTokens != 4 || (name != "unknown" && *got.InputTokens != 10) {
				t.Fatal("partial or known counters changed")
			}
		})
	}
}

func TestInvocationUsageIsBoundToGuardAndActiveInvocation(t *testing.T) {
	g, other := &Guard{}, &Guard{}
	ctx, usage := usageContext(g, "first", "same-session")
	other.RecordInvocationUsage(ctx, "foreign", "judge", &ModelUsage{TotalTokens: measured(999)})
	g.RecordInvocationUsage(context.Background(), "unbound", "model", &ModelUsage{TotalTokens: measured(999)})
	if usage.finish() != nil {
		t.Fatal("foreign guard or unbound context contributed usage")
	}
	g.RecordInvocationUsage(ctx, "late", "model", &ModelUsage{TotalTokens: measured(999)})
	next, second := usageContext(g, "second", "same-session")
	g.RecordInvocationUsage(next, "late", "model", &ModelUsage{TotalTokens: measured(0)})
	got := second.finish()
	if got == nil || got.InvocationID != "second" || got.ModelCalls != 1 || *got.TotalTokens != 0 {
		t.Fatal("late usage leaked into the next invocation")
	}
}

func TestInvocationUsageConcurrentObservationsAreSafe(t *testing.T) {
	g := &Guard{}
	ctx, usage := usageContext(g, "turn", "session")
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			g.RecordInvocationUsage(ctx, strings.Repeat("x", i+1), "model", &ModelUsage{InputTokens: measured(1), OutputTokens: measured(1)})
		}(i)
	}
	wg.Wait()
	got := usage.finish()
	if got == nil || got.ModelCalls != 64 || *got.TotalTokens != 128 {
		t.Fatalf("concurrent counts changed: %+v", got)
	}
}

func TestInvocationUsageConcurrentBindingsRemainSeparate(t *testing.T) {
	g := &Guard{}
	first, a := usageContext(g, "one", "session-a")
	second, b := usageContext(g, "two", "session-b")
	var wg sync.WaitGroup
	for _, ctx := range []context.Context{first, second} {
		wg.Add(1)
		go func(ctx context.Context) {
			defer wg.Done()
			for i := 0; i < 32; i++ {
				g.RecordInvocationUsage(ctx, string(rune(0x100+i)), "model", &ModelUsage{TotalTokens: measured(1)})
			}
		}(ctx)
	}
	wg.Wait()
	for i, scope := range []*invocationUsage{a, b} {
		got := scope.finish()
		if got == nil || got.ModelCalls != 32 || *got.TotalTokens != 32 || (i == 0 && got.InvocationID != "one") || (i == 1 && got.InvocationID != "two") {
			t.Fatalf("concurrent invocations contaminated one another: %+v", got)
		}
	}
	_, abandoned := usageContext(g, "abandoned", "session")
	abandoned.abandon()
	if abandoned.finish() != nil || abandoned.models != nil || abandoned.seen != nil {
		t.Fatal("disconnected invocation retained collector state")
	}
}

func TestInvocationUsageBoundsFailClosed(t *testing.T) {
	for _, name := range []string{"empty-id", "long-id", "control-id", "long-model", "padded-model", "control-model", "overflow", "bad-cache", "bad-reasoning", "bad-total", "aggregate-cache", "aggregate-reasoning", "sum-overflow", "call-limit", "model-limit"} {
		t.Run(name, func(t *testing.T) {
			g := &Guard{}
			ctx, usage := usageContext(g, "turn", "session")
			id, model, counts := "call", "model", &ModelUsage{InputTokens: measured(1), OutputTokens: measured(1)}
			switch name {
			case "empty-id":
				id = ""
			case "long-id":
				id = strings.Repeat("x", 129)
			case "control-id":
				id = "call\n"
			case "long-model":
				model = strings.Repeat("m", 257)
			case "padded-model":
				model = " model"
			case "control-model":
				model = "model\n"
			case "overflow":
				counts.TotalTokens = measured(1 << 53)
			case "bad-cache":
				counts.CachedInputTokens = measured(2)
			case "bad-reasoning":
				counts.ReasoningTokens = measured(2)
			case "bad-total":
				counts.TotalTokens = measured(1)
			case "aggregate-cache":
				g.RecordInvocationUsage(ctx, "partial", "model", &ModelUsage{CachedInputTokens: measured(10)})
			case "aggregate-reasoning":
				g.RecordInvocationUsage(ctx, "partial", "model", &ModelUsage{ReasoningTokens: measured(10)})
			case "sum-overflow":
				counts = &ModelUsage{TotalTokens: measured((1 << 53) - 1)}
				g.RecordInvocationUsage(ctx, "first", "model", counts)
			case "call-limit", "model-limit":
				limit := 257
				if name == "model-limit" {
					limit = 65
				}
				for i := 0; i < limit; i++ {
					label := string(rune(0x100 + i))
					rowModel := "model"
					if name == "model-limit" {
						rowModel = label
					}
					g.RecordInvocationUsage(ctx, label, rowModel, nil)
				}
			}
			g.RecordInvocationUsage(ctx, id, model, counts)
			if got := usage.finish(); got != nil {
				t.Fatalf("invalid usage was emitted: %+v", got)
			}
		})
	}
}

func TestInvocationUsageUnknownFieldsAreExplicitNull(t *testing.T) {
	g := &Guard{}
	ctx, usage := usageContext(g, "turn", "session")
	g.RecordInvocationUsage(ctx, "call", "", nil)
	raw, err := json.Marshal(usage.finish())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"input_tokens", "cached_input_tokens", "output_tokens", "reasoning_tokens", "total_tokens"} {
		value, present := got[field]
		if !present || value != nil {
			t.Fatalf("unknown field %s was fabricated or omitted", field)
		}
	}
}
