// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

// Trace propagation. A Check gets its own span under either the caller's trace
// (supplied via ContextWithTrace) or a session-derived trace, so prism nests the
// check in its lifecycle span tree and the emitted telemetry shares the ids. This
// is the explicit, dependency-free equivalent of the Python SDK's OTel context
// propagation — no opentelemetry import; the caller hands us ids if it has them.

type traceCtxKey struct{}

type traceCtx struct {
	traceID string // 32 hex
	spanID  string // 16 hex — the caller's current span (parent of the SDK's check span)
}

// ContextWithTrace attaches the caller's W3C trace so a Check and its telemetry
// nest under the caller's span. traceID must be 32 hex chars, spanID 16 hex.
func ContextWithTrace(ctx context.Context, traceID, spanID string) context.Context {
	return context.WithValue(ctx, traceCtxKey{}, traceCtx{traceID: traceID, spanID: spanID})
}

func traceFromContext(ctx context.Context) (traceID, parentSpan string, ok bool) {
	if t, has := ctx.Value(traceCtxKey{}).(traceCtx); has {
		return t.traceID, t.spanID, true
	}
	return "", "", false
}

// deriveTraceID makes a stable 32-hex trace id from a session id, so a session's
// checks share a trace even when the caller supplies no OTel context.
func deriveTraceID(session string) string {
	if session == "" {
		return randHexN(16)
	}
	sum := sha256.Sum256([]byte("flyedge-trace:" + session))
	return hex.EncodeToString(sum[:16]) // 16 bytes -> 32 hex
}

func newSpanID() string { return randHexN(8) } // 8 bytes -> 16 hex

func randHexN(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func formatTraceparent(traceID, spanID string) string {
	return "00-" + traceID + "-" + spanID + "-01"
}
