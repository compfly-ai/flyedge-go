// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"context"
	"strings"
	"testing"

	"github.com/compfly-ai/flyedge-go/enforce"
)

func TestDeriveTraceIDStable(t *testing.T) {
	a := deriveTraceID("sess-1")
	if a != deriveTraceID("sess-1") || len(a) != 32 {
		t.Fatalf("session-derived trace id must be stable + 32 hex: %q", a)
	}
	if deriveTraceID("sess-2") == a {
		t.Fatalf("different sessions must derive different trace ids")
	}
}

func TestTraceparentPropagation(t *testing.T) {
	tp := formatTraceparent("0123456789abcdef0123456789abcdef", "0123456789abcdef")
	if !strings.HasPrefix(tp, "00-") || strings.Count(tp, "-") != 3 {
		t.Fatalf("bad traceparent: %q", tp)
	}
	// ContextWithTraceparent must surface as the header prism reads.
	ctx := enforce.ContextWithTraceparent(context.Background(), tp)
	if got := enforce.IdentityHeaders(ctx)["traceparent"]; got != tp {
		t.Fatalf("traceparent not propagated: %q", got)
	}
	// ContextWithTrace round-trips.
	if tid, sp, ok := traceFromContext(ContextWithTrace(context.Background(), "tr", "sp")); !ok || tid != "tr" || sp != "sp" {
		t.Fatalf("ContextWithTrace round-trip failed: %q %q %v", tid, sp, ok)
	}
}
