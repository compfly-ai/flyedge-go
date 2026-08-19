// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import "context"

type sessionKey struct{}

// ContextWithSession returns a context carrying an explicit flyedge session id. The transport wrap
// (and any Check that reads it) uses this id for multi-turn correlation, so a proxy or a
// per-conversation agent can scope sessions per request instead of per client. Empty id is ignored.
func ContextWithSession(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKey{}, id)
}

// sessionFromContext returns the explicit session id set via ContextWithSession, or "".
func sessionFromContext(ctx context.Context) string {
	id, _ := ctx.Value(sessionKey{}).(string)
	return id
}
