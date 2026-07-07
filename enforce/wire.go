// Package enforce is the policy decision-point client: it signs a CheckRequest and POSTs it to
// prism's /v1/flyedge/check, returning a typed Decision. The JSON shapes here are the frozen wire
// schema (DESIGN.md §1b) shared with prism/policy-enforcer and the Python/TS SDKs.
package enforce

import (
	"crypto/sha256"
	"encoding/hex"
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

// CheckRequest is the body POSTed to /v1/flyedge/check. The signature is computed over the exact
// serialized bytes of this struct, so serialize once and sign those bytes.
type CheckRequest struct {
	RequestID     string         `json:"request_id"`
	SessionID     string         `json:"session_id"`
	TimestampMS   int64          `json:"timestamp_ms"`
	Stage         Stage          `json:"stage"`
	ComponentType string         `json:"component_type"` // e.g. LLM, TOOL
	ComponentName string         `json:"component_name"` // e.g. ChatAnthropic
	MethodName    string         `json:"method_name"`    // e.g. invoke
	Content       Content        `json:"content"`
	Operation     Operation      `json:"operation"`
	Metadata      map[string]any `json:"metadata,omitempty"`
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
	ModelID      string `json:"model_id,omitempty"`
	DestDomain   string `json:"dest_domain,omitempty"`
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
}

// checkResponse is the raw /check response envelope.
type checkResponse struct {
	Decision       string   `json:"decision"`
	Reason         string   `json:"reason"`
	Message        string   `json:"message"`
	Warnings       []string `json:"warnings"`
	PolicyVersion  string   `json:"policy_version"`
	RequestID      string   `json:"request_id"`
	SignalsPresent []string `json:"signals_present"`
	SignalsMissing []string `json:"signals_missing"`
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
	}
}
