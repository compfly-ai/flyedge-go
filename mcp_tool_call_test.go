// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"context"
	"testing"
)

func TestCheckMCPToolCallSendsServerEndpoint(t *testing.T) {
	rec := &mediaChecker{decision: Decision{Action: ActionAllow}}
	g := mediaGuard(t, rec, FailOpen)
	args := map[string]any{"owner": "acme", "repo": "claims"}
	if _, err := g.CheckMCPToolCall(context.Background(), "s", "mcp__github__create_issue", "https://api.githubcopilot.com/mcp", args); err != nil {
		t.Fatal(err)
	}
	op := rec.requests[0].Operation
	if rec.requests[0].Stage != StageToolCall || op.Type != "tool.call" || op.ToolName != "mcp__github__create_issue" {
		t.Fatalf("not a tool_call check: stage=%s op=%+v", rec.requests[0].Stage, op)
	}
	if op.MCPServerID != "https://api.githubcopilot.com/mcp" {
		t.Fatalf("mcp_server_id = %q", op.MCPServerID)
	}
	if op.ToolArgsJSON != `{"owner":"acme","repo":"claims"}` {
		t.Fatalf("tool args = %s", op.ToolArgsJSON)
	}
}

func TestCheckToolCallLeavesServerEndpointEmpty(t *testing.T) {
	rec := &mediaChecker{decision: Decision{Action: ActionAllow}}
	g := mediaGuard(t, rec, FailOpen)
	if _, err := g.CheckToolCall(context.Background(), "s", "Bash", map[string]any{"command": "ls"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.requests[0].Operation.MCPServerID; got != "" {
		t.Fatalf("mcp_server_id = %q", got)
	}
}
