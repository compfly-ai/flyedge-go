package enforce

import (
	"context"
	"encoding/base64"
	"encoding/json"
)

// Identity-attribution headers prism reads on /v1/flyedge/check. These are NOT validated credentials
// on their own — prism trusts them on the strength of the DID-signed channel (the Ed25519 signature
// covers the body + timestamp, not the header set); the real credential is the raw token in the
// request body. The names + value formats are the frozen wire contract, verified against
// prism.
const (
	// HeaderOBOPrincipal carries the on-behalf-of envelope as base64url(JSON) with NO padding
	// (prism decodes URL_SAFE_NO_PAD). prism extracts provider/upn (+ urn/scope) for governance.
	HeaderOBOPrincipal = "X-CompFly-OBO-Principal"
	// HeaderDelegationToken carries a compact EdDSA JWT (typ "delegation-token"); prism forwards it
	// to the token-service to verify and unpacks the mandate chain. The SDK just attaches the token.
	HeaderDelegationToken = "X-CompFly-Delegation-Token"
	// HeaderAgentSID is the non-human-identity subject id (plain string).
	HeaderAgentSID = "X-CompFly-Agent-SID"
	// HeaderAgentURN is the structured NHI urn (plain string), grammar
	// "compfly:identity:v1:<provider>:<id_type>:<k>=<v>,...".
	HeaderAgentURN = "X-CompFly-Agent-URN"
)

// Principal is the on-behalf-of envelope: the end-user an agent is acting for on a given request. It
// maps to prism's OBO header (base64url JSON). prism plucks Provider/UPN (+ URN/Scope) for policy;
// the remaining fields are audit metadata. The envelope is an extraction hint — the actual credential
// is the raw OBO token in the request body — so a full production deployment sets both this and the
// underlying token. Field names match the platform envelope (camelCase).
type Principal struct {
	Provider string `json:"provider,omitempty"`
	TenantID string `json:"tenantId,omitempty"`
	OBOID    string `json:"oboId,omitempty"`
	UPN      string `json:"upn,omitempty"`
	URN      string `json:"urn,omitempty"`
	// Scope is the principal's claim map (role, plan, tenant, groups, …) — the attribute bag policy
	// keys on. It is sent as a JSON OBJECT (prism serializes it into obo_scope_json for the policy
	// engine), NOT a pre-encoded string, so a rule reads obo.scope.<claim> directly.
	Scope       map[string]string `json:"scope,omitempty"`
	TokenHash   string            `json:"tokenHash,omitempty"`
	Issuer      string            `json:"issuer,omitempty"`
	Algorithm   string            `json:"algorithm,omitempty"`
	KeyID       string            `json:"keyId,omitempty"`
	PresentedAt string            `json:"presentedAt,omitempty"`
	ExpiresAt   string            `json:"expiresAt,omitempty"`
}

// encode returns the base64url(JSON) header value, no padding — matching prism's URL_SAFE_NO_PAD
// decode. Returns "" for the zero principal (nothing to attach). JSON object key order is irrelevant
// to prism (it parses into a map), so struct-field order is fine.
func (p Principal) encode() string {
	b, err := json.Marshal(p)
	if err != nil || string(b) == "{}" { // omitempty ⇒ an empty principal marshals to "{}"
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

type (
	principalKey   struct{}
	delegationKey  struct{}
	agentIDKey     struct{}
	traceparentKey struct{}
)

// agentIdentity holds the NHI subject id + structured urn set via ContextWithAgentIdentity.
type agentIdentity struct{ sid, urn string }

// ContextWithPrincipal returns a context carrying the end-user an agent is acting on behalf of. The
// enforcer attaches it as the OBO header on every signed request made with this context. The zero
// Principal is ignored. This is the mechanism a single served agent uses to govern per-user: set the
// principal for the served request, and the gateway sees who each action is for.
func ContextWithPrincipal(ctx context.Context, p Principal) context.Context {
	if p.encode() == "" { // nothing to attach (empty principal)
		return ctx
	}
	return context.WithValue(ctx, principalKey{}, p)
}

// ContextWithDelegation returns a context carrying a raw delegation-token JWT (agent-to-agent
// authority). The enforcer attaches it as the delegation header; prism verifies it and unpacks the
// intent/task mandate chain encoded inside the token. Empty token is ignored.
func ContextWithDelegation(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, delegationKey{}, token)
}

// ContextWithAgentIdentity returns a context carrying the agent's non-human-identity attribution:
// sid (subject id) and/or urn (structured identity). Both empty is ignored. These ride alongside the
// crypto DID (set by the signer) as extra attribution for the acting agent.
func ContextWithAgentIdentity(ctx context.Context, sid, urn string) context.Context {
	if sid == "" && urn == "" {
		return ctx
	}
	return context.WithValue(ctx, agentIDKey{}, agentIdentity{sid: sid, urn: urn})
}

// IdentityHeaders returns the identity-attribution headers carried on ctx, ready to attach to a
// signed request. Empty map when none are set. These are headers only — never part of the signed
// body — so they never disturb the frozen /check request schema or its signature.
func IdentityHeaders(ctx context.Context) map[string]string {
	h := map[string]string{}
	if p, ok := ctx.Value(principalKey{}).(Principal); ok {
		if v := p.encode(); v != "" {
			h[HeaderOBOPrincipal] = v
		}
	}
	if tok, ok := ctx.Value(delegationKey{}).(string); ok && tok != "" {
		h[HeaderDelegationToken] = tok
	}
	if id, ok := ctx.Value(agentIDKey{}).(agentIdentity); ok {
		if id.sid != "" {
			h[HeaderAgentSID] = id.sid
		}
		if id.urn != "" {
			h[HeaderAgentURN] = id.urn
		}
	}
	if tp, ok := ctx.Value(traceparentKey{}).(string); ok && tp != "" {
		h["traceparent"] = tp
	}
	return h
}

// ContextWithTraceparent attaches a W3C `traceparent` so the signed POSTs carry it.
// prism reads it (field[1]=trace id, field[2]=parent span) to place the check in its
// lifecycle span tree. Empty is ignored.
func ContextWithTraceparent(ctx context.Context, traceparent string) context.Context {
	if traceparent == "" {
		return ctx
	}
	return context.WithValue(ctx, traceparentKey{}, traceparent)
}
