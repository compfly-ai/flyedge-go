// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CompFly AI

package flyedge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
)

const maxProxyPolicyBody = 64 * 1024
const maxProxyWarningHeader = 16 * 1024

// Only proxyForward calls this: a direct provider cannot assert CompFly policy.
// Successful bodies (including SSE) are never read to deliver warning metadata.
func (t *guardRoundTripper) proxyPolicyResponse(resp *http.Response, check CheckRequest) (*http.Response, error) {
	if resp.StatusCode == http.StatusForbidden && resp.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProxyPolicyBody+1))
		if err != nil {
			resp.Body.Close()
			return nil, err
		}
		var envelope struct {
			Error *struct {
				Type     string   `json:"type"`
				Message  *string  `json:"message"`
				Reason   string   `json:"reason"`
				Warnings []string `json:"warnings"`
			} `json:"error"`
		}
		if len(raw) <= maxProxyPolicyBody && json.Unmarshal(raw, &envelope) == nil && envelope.Error != nil && envelope.Error.Type == "policy_denied" && envelope.Error.Message != nil {
			resp.Body.Close()
			d := Decision{Action: ActionDeny, Message: *envelope.Error.Message, Reason: envelope.Error.Reason, Warnings: envelope.Error.Warnings}
			if t.guard.onDecision != nil {
				t.guard.onDecision(check, d)
			}
			return nil, &DenyError{Decision: d}
		}
		// Preserve non-policy errors, including bodies exceeding the inspection bound.
		resp.Body = &proxyReplayBody{Reader: io.MultiReader(bytes.NewReader(raw), resp.Body), Closer: resp.Body}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && t.guard.onDecision != nil {
		values := resp.Header.Values("X-CompFly-Policy-Warnings")
		if len(values) == 1 && len(values[0]) <= maxProxyWarningHeader {
			raw, err := base64.StdEncoding.DecodeString(values[0])
			var warnings []struct {
				Stage    Stage     `json:"stage"`
				Message  *string   `json:"message"`
				Reason   *string   `json:"reason"`
				Warnings []*string `json:"warnings"`
			}
			if err == nil && json.Unmarshal(raw, &warnings) == nil {
				// Validate the entire envelope before invoking callbacks.
				for _, w := range warnings {
					if (w.Stage != StagePreLLM && w.Stage != StagePostLLM) || w.Message == nil || w.Reason == nil || len(w.Warnings) == 0 {
						return resp, nil
					}
					for _, id := range w.Warnings {
						if id == nil {
							return resp, nil
						}
					}
				}
				for _, w := range warnings {
					ids := make([]string, len(w.Warnings))
					for i, id := range w.Warnings {
						ids[i] = *id
					}
					check.Stage = w.Stage
					t.guard.onDecision(check, Decision{Action: ActionWarn, Message: *w.Message, Reason: *w.Reason, Warnings: ids})
				}
			}
		}
	}
	return resp, nil
}

type proxyReplayBody struct {
	io.Reader
	io.Closer
}
