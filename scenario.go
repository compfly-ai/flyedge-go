// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CompFly AI
package flyedge

import (
	"context"
	"encoding/json"
	"fmt"
)

// BeforeScenarioTool implements a scenario-owned fixture without executing the
// real tool. Call AFTER CheckToolCall, execute only when mocked is false, and
// govern either result with GovernToolResult. RecordToolIO is for real executions
// only. allowMock is deployment authorization, never a user-supplied flag.
func (g *Guard) BeforeScenarioTool(ctx context.Context, session, tool string, args any, allowMock bool) (value json.RawMessage, mocked bool, err error) {
	if g == nil || g.simCtl == nil {
		return nil, false, nil
	}
	if err = ctx.Err(); err != nil {
		return nil, false, err
	}
	if scope, ok := ctx.Value(invocationContextKey{}).(invocationScope); ok {
		if (!g.simCtl.HasExecution(session) && g.simCtl.Active() && scope.runID != g.simCtl.RunID()) || session != sessionFromContext(ctx) {
			return nil, false, fmt.Errorf("scenario mock run mismatch")
		}
		allowMock = allowMock && scope.allowInjection
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, false, err
	}
	var normalized any
	if err = json.Unmarshal(b, &normalized); err != nil {
		return nil, false, err
	}
	return g.simCtl.BeforeScenarioTool(session, tool, normalized, allowMock)
}

// ScenarioMockStatus returns installation and match evidence, not a run verdict.
func (g *Guard) ScenarioMockStatus() map[string]any {
	if g == nil || g.simCtl == nil {
		return map[string]any{"plan_id": nil}
	}
	return g.simCtl.ScenarioStatus()
}
