package flyedge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Session taints. prism accrues per-session signals (prompt-injection, PII, etc.) as
// the session runs; a caller can read the current taint state and acknowledge it. These
// hit GET /v1/sessions/{id}/taint and POST /v1/sessions/{id}/taint/acknowledge, signed
// like every other flyedge call.

// Taint is prism's SessionTaintDocument: the rolled-up taint state for a session.
// SessionSeverity is the aggregate score; Taints holds the individual signals.
type Taint struct {
	Version         int          `json:"version"`
	NamespaceID     string       `json:"namespace_id"`
	SessionID       string       `json:"session_id"`
	Taints          []TaintEntry `json:"taints"`
	SessionSeverity float64      `json:"session_severity"`
	TaintHash       string       `json:"taint_hash"`
	CurrentTurn     int          `json:"current_turn"`
}

// TaintEntry is one accrued taint signal (prism TaintEntry).
type TaintEntry struct {
	ToolCallID     string   `json:"tool_call_id,omitempty"`
	ToolName       string   `json:"tool_name,omitempty"`
	Labels         []string `json:"labels,omitempty"`
	InjectionScore *float64 `json:"injection_score,omitempty"`
	PIIScore       *float64 `json:"pii_score,omitempty"`
	Turn           int      `json:"turn"`
	Severity       float64  `json:"severity"`
	CreatedAt      string   `json:"created_at"`
}

// signedRoundTripper is the subset of the enforcer the taint calls need. The default
// HTTPEnforcer satisfies it; an injected enforcer that doesn't yields a clear error.
type signedRoundTripper interface {
	GetSigned(ctx context.Context, path string, headers map[string]string) ([]byte, error)
	PostSigned(ctx context.Context, path string, body []byte) ([]byte, error)
}

// SessionTaint reads the current taint state for a session. Returns (nil, nil) when the
// session has no taint (prism 404s an untainted session). A returned Taint with a
// non-zero SessionSeverity means earlier steps tripped injection/PII/etc. signals —
// callers can gate on it (e.g. refuse autonomous actions above a threshold).
func (g *Guard) SessionTaint(ctx context.Context, sessionID string) (*Taint, error) {
	rt, ok := g.enforcer.(signedRoundTripper)
	if !ok {
		return nil, fmt.Errorf("flyedge: enforcer does not support signed taint requests")
	}
	raw, err := rt.GetSigned(ctx, "/v1/sessions/"+url.PathEscape(sessionID)+"/taint", nil)
	if err != nil {
		if strings.Contains(err.Error(), "→ 404") {
			return nil, nil // untainted session
		}
		return nil, err
	}
	var t Taint
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, fmt.Errorf("flyedge: decode taint: %w", err)
	}
	return &t, nil
}

// AcknowledgeSessionTaint acknowledges all taints on a session
// (POST /v1/sessions/{id}/taint/acknowledge), clearing the taint gate.
func (g *Guard) AcknowledgeSessionTaint(ctx context.Context, sessionID string) error {
	rt, ok := g.enforcer.(signedRoundTripper)
	if !ok {
		return fmt.Errorf("flyedge: enforcer does not support signed taint requests")
	}
	_, err := rt.PostSigned(ctx, "/v1/sessions/"+url.PathEscape(sessionID)+"/taint/acknowledge", []byte("{}"))
	return err
}
