// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import "context"

// GovernToolResult gates a tool result AND returns the content the caller should feed back to the
// model. It is the value-returning companion to CheckToolResponse: use it wherever the tool result
// flows onward, because the governed content may differ from the raw result —
//
//   - During an active simulation in attack mode, the attack injector may MUTATE the result
//     (tool_poison merges adversarial fields; error_inject replaces it with a crafted error). The
//     injection is emitted as telemetry for the eval harness's 4-state outcome correlation.
//   - The result is always run through the tool_call_response check (enforcement + telemetry). A
//     policy denial returns a *DenyError — the caller should withhold the result from the model.
//
// This is the seam Phase B2 needs for injection and that a production agent needs for response
// redaction: one place where the response content can be transformed, not merely allowed/denied.
func (g *Guard) GovernToolResult(ctx context.Context, session, toolName, result string) (string, Decision, error) {
	out := result
	if g.simCtl != nil {
		if mutated, injected := g.simCtl.InjectToolResult(toolName, result); injected {
			out = mutated
		}
	}
	dec, err := g.CheckToolResponse(ctx, session, toolName, out)
	return out, dec, err
}
