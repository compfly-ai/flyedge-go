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
		return Decision{}, fmt.Errorf("enforce: %s → %d: %s", checkPath, resp.StatusCode, string(raw))
	}

	var cr checkResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Decision{}, fmt.Errorf("enforce: decode response: %w (body: %s)", err, string(raw))
	}
	return cr.normalize(), nil
}
