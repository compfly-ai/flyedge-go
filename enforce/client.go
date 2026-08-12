package enforce

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/compfly-ai/flyedge-go/identity"
)

// checkPath is the enforcement endpoint on the prism gateway.
const checkPath = "/v1/flyedge/check"

// applyIdentityHeaders attaches any OBO/delegation/agent-identity headers carried on ctx to a
// signed request. These are headers only — they are never folded into the signed body — so the
// frozen /check request schema and its signature are untouched. No-op when ctx carries none.
func applyIdentityHeaders(ctx context.Context, req *http.Request) {
	for k, v := range IdentityHeaders(ctx) {
		req.Header.Set(k, v)
	}
}

// Enforcer is the policy decision point. Implementations are swappable (HTTP client, an offline
// stub for tests, a record/replay fake). Check must be safe for concurrent use.
type Enforcer interface {
	Check(ctx context.Context, req CheckRequest) (Decision, error)
}

// HTTPEnforcer calls prism /v1/flyedge/check over HTTP, signing the request body with a Signer.
type HTTPEnforcer struct {
	baseURL string
	signer  identity.Signer // may be nil (unsigned check-only against a permissive endpoint)
	hc      *http.Client
	now     func() time.Time
}

// NewHTTPEnforcer builds an enforcer. baseURL is the prism base (no trailing slash needed);
// signer may be nil; timeout bounds each call.
func NewHTTPEnforcer(baseURL string, signer identity.Signer, timeout time.Duration) *HTTPEnforcer {
	return &HTTPEnforcer{
		baseURL: baseURL,
		signer:  signer,
		hc:      &http.Client{Timeout: timeout},
		now:     time.Now,
	}
}

// Check serializes req, signs the exact bytes, POSTs to /v1/flyedge/check, and returns the typed
// Decision. A non-2xx or transport error is returned as an error — the caller (Guard) decides
// fail-open vs fail-closed; this layer does not silently allow.
func (e *HTTPEnforcer) Check(ctx context.Context, req CheckRequest) (Decision, error) {
	req.fillDefaults()
	body, err := json.Marshal(req)
	if err != nil {
		return Decision{}, fmt.Errorf("enforce: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+checkPath, bytes.NewReader(body))
	if err != nil {
		return Decision{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	applyIdentityHeaders(ctx, httpReq)
	if e.signer != nil {
		hdrs, err := e.signer.Sign(body, e.now())
		if err != nil {
			return Decision{}, fmt.Errorf("enforce: sign: %w", err)
		}
		for k, v := range hdrs {
			httpReq.Header.Set(k, v)
		}
	}

	resp, err := e.hc.Do(httpReq)
	if err != nil {
		return Decision{}, fmt.Errorf("enforce: call %s: %w", checkPath, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A full-scope kill switch arrives as 403 {code:"KILL_SWITCH", details:{...}}. Surface it
		// as a typed KilledError so the Guard can always block it, bypassing fail-open.
		if resp.StatusCode == http.StatusForbidden {
			if ke := parseKillError(raw); ke != nil {
				return Decision{}, ke
			}
		}
		return Decision{}, fmt.Errorf("enforce: %s → %d: %s", checkPath, resp.StatusCode, string(raw))
	}

	var cr checkResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Decision{}, fmt.Errorf("enforce: decode response: %w (body: %s)", err, string(raw))
	}
	return cr.normalize(), nil
}

// KilledError is returned by Check when the gateway rejects a request with a full-scope kill
// switch (HTTP 403 code=KILL_SWITCH). It is distinct from a policy deny so the Guard can enforce it
// unconditionally (a kill must never be bypassed by fail-open).
type KilledError struct{ Kill KillInfo }

func (e *KilledError) Error() string {
	return "flyedge: kill switch active: " + e.Kill.Reason
}

// parseKillError returns a *KilledError if the 403 body is a KILL_SWITCH error, else nil.
func parseKillError(raw []byte) *KilledError {
	var fe struct {
		Code    string `json:"code"`
		Details struct {
			KillID string `json:"kill_id"`
			Scope  string `json:"scope"`
			Target string `json:"target"`
			Reason string `json:"reason"`
		} `json:"details"`
	}
	if json.Unmarshal(raw, &fe) != nil || fe.Code != "KILL_SWITCH" {
		return nil
	}
	return &KilledError{Kill: KillInfo{
		KillID: fe.Details.KillID, Scope: fe.Details.Scope,
		Target: fe.Details.Target, Reason: fe.Details.Reason,
	}}
}

// GetSigned signs and GETs path (empty body), attaching extra request headers (e.g. the X-Agent-*
// heartbeat headers on /v1/flyedge/config). The signature is computed over the empty body, matching
// prism's SHA-256(ts‖body) scheme for a body-less request. A non-2xx is an error.
func (e *HTTPEnforcer) GetSigned(ctx context.Context, path string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	applyIdentityHeaders(ctx, req)
	if e.signer != nil {
		hdrs, err := e.signer.Sign(nil, e.now())
		if err != nil {
			return nil, fmt.Errorf("enforce: sign: %w", err)
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enforce: call %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("enforce: %s → %d: %s", path, resp.StatusCode, string(raw))
	}
	return raw, nil
}

// GetSignedConditional is GetSigned with 304 handling: a Not Modified response comes back as
// (nil, true, nil) rather than an error. Plain GetSigned treats every non-2xx as a failure, which
// is right for endpoints where 304 is meaningless — this variant exists for poll endpoints that
// send If-None-Match and want "nothing changed" as an ordinary outcome, not an error to swallow.
func (e *HTTPEnforcer) GetSignedConditional(ctx context.Context, path string, headers map[string]string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+path, nil)
	if err != nil {
		return nil, false, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	applyIdentityHeaders(ctx, req)
	if e.signer != nil {
		hdrs, err := e.signer.Sign(nil, e.now())
		if err != nil {
			return nil, false, fmt.Errorf("enforce: sign: %w", err)
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("enforce: call %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, true, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("enforce: %s → %d: %s", path, resp.StatusCode, string(raw))
	}
	return raw, false, nil
}

// PostSigned signs and POSTs body to an arbitrary flyedge path (e.g. /v1/flyedge/connect,
// /v1/flyedge/telemetry), returning the response bytes. Reuses the same signing as Check so the
// connect + telemetry lifecycle calls authenticate identically. A non-2xx is an error.
func (e *HTTPEnforcer) PostSigned(ctx context.Context, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyIdentityHeaders(ctx, req)
	if e.signer != nil {
		hdrs, err := e.signer.Sign(body, e.now())
		if err != nil {
			return nil, fmt.Errorf("enforce: sign: %w", err)
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("enforce: call %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("enforce: %s → %d: %s", path, resp.StatusCode, string(raw))
	}
	return raw, nil
}
