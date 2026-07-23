package flyedge

import (
	"context"

	"github.com/compfly-ai/flyedge-go/enforce"
)

// Principal is the on-behalf-of envelope — the end-user an agent is acting for on a request.
// Re-exported from enforce so callers depend only on the flyedge package. See ContextWithPrincipal.
type Principal = enforce.Principal

// ContextWithPrincipal returns a context carrying the end-user an agent is acting on behalf of.
// Every governed call (Check*, the WrapRoundTripper LLM path, Connect, telemetry) made with this
// context attaches the OBO header, so a single served agent identity can be governed per-user at the
// gateway. The zero Principal is ignored.
//
// A production deployment also passes the underlying credential (the raw OBO token) in the request
// body it forwards to the provider; this envelope is prism's extraction/attribution hint.
func ContextWithPrincipal(ctx context.Context, p Principal) context.Context {
	return enforce.ContextWithPrincipal(ctx, p)
}

// ContextWithDelegation returns a context carrying a raw delegation-token JWT (agent-to-agent
// authority). prism verifies it and unpacks the intent/task mandate chain encoded inside the token.
// Empty token is ignored.
func ContextWithDelegation(ctx context.Context, token string) context.Context {
	return enforce.ContextWithDelegation(ctx, token)
}

// ContextWithAgentIdentity returns a context carrying the acting agent's non-human-identity
// attribution: sid (subject id) and/or urn (structured identity). Both empty is ignored. These ride
// alongside the crypto DID (set by the signer) as additional attribution.
func ContextWithAgentIdentity(ctx context.Context, sid, urn string) context.Context {
	return enforce.ContextWithAgentIdentity(ctx, sid, urn)
}
