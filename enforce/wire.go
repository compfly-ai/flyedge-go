// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Package enforce is the policy decision-point client: it signs a CheckRequest and POSTs it to
// prism's /v1/flyedge/check, returning a typed Decision. The JSON shapes here are the frozen wire
// schema (DESIGN.md §1b) shared with prism/policy-enforcer and the Python/TS SDKs.
package enforce

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

func timeNowMS() int64 { return time.Now().UnixMilli() }

// Stage is the enforcement point in an agent turn. Values are wire strings — do not change.
type Stage string

const (
	StagePreLLM           Stage = "pre_llm"
	StageToolCall         Stage = "tool_call"
	StageToolCallResponse Stage = "tool_call_response"
	StagePostLLM          Stage = "post_llm"
)

// Action is the normalized decision. Block folds into Deny for callers; Warn is advisory.
type Action string

const (
	ActionAllow Action = "allow"
	ActionDeny  Action = "deny"
	ActionWarn  Action = "warn"
)

// OriginType values for CheckRequest.OriginType (prism FlyedgeOriginType, snake_case).
// Use one of these — prism rejects any other value (it deserializes into a fixed enum).
const (
	OriginTypeUser       = "user"       // direct user request (human in the loop)
	OriginTypeAgent      = "agent"      // agent-mediated request (default)
	OriginTypeAutonomous = "autonomous" // fully autonomous, no user context
)

// CheckRequest is the body POSTed to /v1/flyedge/check. The signature is computed over the exact
// serialized bytes of this struct, so serialize once and sign those bytes.
type CheckRequest struct {
	RequestID     string    `json:"request_id"`
	SessionID     string    `json:"session_id"`
	TimestampMS   int64     `json:"timestamp_ms"`
	Stage         Stage     `json:"stage"`
	ComponentType string    `json:"component_type"` // e.g. LLM, TOOL
	ComponentName string    `json:"component_name"` // e.g. ChatAnthropic
	MethodName    string    `json:"method_name"`    // e.g. invoke
	Content       Content   `json:"content"`
	Operation     Operation `json:"operation"`
	// Enrichment context prism accepts and feeds to policy. All optional — unset
	// fields fall back to prism's serde defaults. Framework identifies the SDK.
	Framework        string            `json:"framework,omitempty"`
	Layer            string            `json:"layer,omitempty"`
	Provider         string            `json:"provider,omitempty"`
	OriginType       string            `json:"origin_type,omitempty"`
	ExecutionContext *ExecutionContext `json:"execution_context,omitempty"`
	AuthContext      *AuthContext      `json:"auth_context,omitempty"`
	Metadata         map[string]any    `json:"metadata,omitempty"`
}

// ExecutionContext describes how/where the operation runs (prism FlyedgeExecutionContext) —
// feeds environment/autonomy/trigger-aware policy. Optional.
type ExecutionContext struct {
	Environment  string `json:"environment,omitempty"`
	IsAutonomous bool   `json:"is_autonomous,omitempty"`
	TriggerType  string `json:"trigger_type,omitempty"`
	Scheduled    bool   `json:"scheduled,omitempty"`
	EventDriven  bool   `json:"event_driven,omitempty"`
}

// AuthContext carries application auth attributes for attribute-based governance
// (prism FlyedgeAuthContext). Optional; pointers distinguish unset from zero.
type AuthContext struct {
	Method            string   `json:"method,omitempty"`
	UserGroups        []string `json:"user_groups,omitempty"`
	Department        string   `json:"department,omitempty"`
	ClearanceLevel    string   `json:"clearance_level,omitempty"`
	LastAuthMinutes   *int64   `json:"last_auth_minutes,omitempty"`
	FailedAttempts    *int     `json:"failed_attempts,omitempty"`
	DeviceID          string   `json:"device_id,omitempty"`
	DeviceTrustScore  *float32 `json:"device_trust_score,omitempty"`
	SessionAgeMinutes *int64   `json:"session_age_minutes,omitempty"`
	RequiresAudit     bool     `json:"requires_audit,omitempty"`
	DataResidency     string   `json:"data_residency,omitempty"`
}

// Content is the payload under inspection. Full carries the text; Hash + SizeBytes are required by
// the gateway and filled automatically from Full when unset (see CheckRequest.fillDefaults).
// Preview is a short excerpt for logs.
type Content struct {
	Preview   string `json:"preview"`
	Full      string `json:"full"`
	Hash      string `json:"hash"`
	SizeBytes int    `json:"size_bytes"`
}

// Operation describes what the agent is doing (chat completion, tool call, …).
type Operation struct {
	Type         string `json:"type"`
	ToolName     string `json:"tool_name,omitempty"`
	ToolArgsHash string `json:"tool_args_hash,omitempty"`
	// ToolArgsJSON carries the full tool arguments as JSON so argument-level policies can evaluate
	// values (prism forwards it to the enforcer's tool_args map). Distinct from Content: Content is
	// the inspected/hashed payload + preview; this is the structured args for policy.
	ToolArgsJSON string `json:"tool_args_json,omitempty"`
	ModelID      string `json:"model_id,omitempty"`
	DestDomain   string `json:"dest_domain,omitempty"`
	MCPServerID  string `json:"mcp_server_id,omitempty"`
}

// fillDefaults populates the gateway-required derived fields the caller can omit: content hash
// (sha256 hex of Full), size, and a short preview. Called by the enforcer before signing so the
// signed bytes and the sent bytes are identical.
func (r *CheckRequest) fillDefaults() {
	if r.TimestampMS == 0 {
		r.TimestampMS = timeNowMS()
	}
	if r.ComponentType == "" {
		r.ComponentType = "LLM"
	}
	if r.ComponentName == "" {
		r.ComponentName = "unknown"
	}
	if r.MethodName == "" {
		r.MethodName = "invoke"
	}
	if r.Content.SizeBytes == 0 {
		r.Content.SizeBytes = len(r.Content.Full)
	}
	if r.Content.Hash == "" {
		sum := sha256.Sum256([]byte(r.Content.Full))
		r.Content.Hash = hex.EncodeToString(sum[:])
	}
	if r.Content.Preview == "" {
		r.Content.Preview = r.Content.Full
		if len(r.Content.Preview) > 200 {
			r.Content.Preview = r.Content.Preview[:200]
		}
	}
	// Correlation id. The transport wrap sets its own per-LLM-call id, but direct Check callers
	// (the stage helpers, the flyedged hook path) previously left this empty, so their decisions
	// landed in the observability layer with no request_id and could not be joined to anything.
	if r.RequestID == "" {
		r.RequestID = "req-" + randRequestHex()
	}
}

// randRequestHex returns 16 hex chars for a synthesized request id.
func randRequestHex() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(strconv.FormatInt(timeNowMS(), 10)))
	}
	return hex.EncodeToString(b[:])
}

// KillInfo describes an active kill switch matching a request. Full-scope kills arrive as a 403
// (see KilledError); non-full (model/tool) kills arrive in a 200 response's `kills` array.
type KillInfo struct {
	KillID string `json:"kill_id"`
	Scope  string `json:"scope"` // full | model | tool | provider
	Target string `json:"target"`
	Reason string `json:"reason"`
}

// checkResponse is the raw /check response envelope.
type checkResponse struct {
	Decision       string     `json:"decision"`
	Reason         string     `json:"reason"`
	Message        string     `json:"message"`
	Warnings       []string   `json:"warnings"`
	PolicyVersion  string     `json:"policy_version"`
	RequestID      string     `json:"request_id"`
	SignalsPresent []string   `json:"signals_present"`
	SignalsMissing []string   `json:"signals_missing"`
	Kills          []KillInfo `json:"kills"`
}

// Decision is the typed, normalized result the caller sees.
type Decision struct {
	Action        Action
	Reason        string
	Message       string
	Warnings      []string
	PolicyVersion string
	RequestID     string
	Signals       Signals
	Kills         []KillInfo // non-full-scope kills matching this request (model/tool)
}

// Signals carries the capability signals the server reported present/missing (audit trail).
type Signals struct {
	Present []string
	Missing []string
}

// normalize maps a raw response into a Decision. deny and block both become ActionDeny.
func (r checkResponse) normalize() Decision {
	act := ActionAllow
	switch r.Decision {
	case "deny", "block":
		act = ActionDeny
	case "warn":
		act = ActionWarn
	}
	return Decision{
		Action:        act,
		Reason:        r.Reason,
		Message:       r.Message,
		Warnings:      r.Warnings,
		PolicyVersion: r.PolicyVersion,
		RequestID:     r.RequestID,
		Signals:       Signals{Present: r.SignalsPresent, Missing: r.SignalsMissing},
		Kills:         r.Kills,
	}
}
