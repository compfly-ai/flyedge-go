// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"context"
	"net/http"
	"regexp"
	"strings"
)

type inboundKey struct{}

var (
	inboundSession = regexp.MustCompile(`^(?:(?:pg-|eval:)[A-Za-z0-9._:-]+|ctest-[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12})$`)
	inboundTrace   = regexp.MustCompile(`^([0-9a-f]{2})-([0-9a-f]{32})-([0-9a-f]{16})-[0-9a-f]{2}$`)
)

// InboundHandler scopes each request with its eval/playground simulation correlation, so checks
// made with the request context use the simulation session in place of the one passed. Wrap the
// server's handler once. Headers provide correlation only, never identity or authority; ordinary
// traffic masks an enclosing scope.
func InboundHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session := singleHeader(r.Header, "X-Simulation-Session-Id")
		if !inboundSession.MatchString(session) {
			session = ""
		}
		ctx := context.WithValue(r.Context(), inboundKey{}, session)
		if session != "" {
			m := inboundTrace.FindStringSubmatch(singleHeader(r.Header, "Traceparent"))
			if m != nil && m[1] != "ff" && m[2] != strings.Repeat("0", 32) && m[3] != strings.Repeat("0", 16) {
				ctx = ContextWithTrace(ctx, m[2], "") // Each check creates its own span.
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func singleHeader(h http.Header, name string) string {
	if values := h.Values(name); len(values) == 1 {
		return values[0]
	}
	return ""
}

func inboundSessionFromContext(ctx context.Context) string {
	session, _ := ctx.Value(inboundKey{}).(string)
	return session
}
