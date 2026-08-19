// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WrapRoundTripper returns an http.RoundTripper that runs a flyedge pre_llm policy check before
// each outbound LLM-provider request, then forwards on Allow. It is provider-agnostic: any HTTP
// LLM client (the Anthropic SDK, the OpenAI SDK, raw net/http) is governed by installing this one
// transport — no per-framework adapter required:
//
//	hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
//	client := anthropic.NewClient(option.WithHTTPClient(hc))   // or openai.NewClient(option.WithHTTPClient(hc))
//
// On a policy Deny the RoundTrip returns a *DenyError (wrapped by net/http in *url.Error, still
// reachable via errors.As), and the provider is never called. Requests to hosts without a
// registered extractor pass through unchecked.
func (g *Guard) WrapRoundTripper(base http.RoundTripper, opts ...WrapOption) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	rt := &guardRoundTripper{guard: g, base: base, session: "sess-" + randHex()}
	for _, o := range opts {
		o(rt)
	}
	return rt
}

// configInjectRewrite inserts an adversarial system message into an LLM request body. It is
// structural (not provider-hardcoded): an Anthropic-style body (top-level `system`) has the message
// prepended to system; an OpenAI-style body (a `messages` array) gets a system message inserted at
// the front; a Gemini-style body (a `contents` array) gets the message prepended to
// `systemInstruction`. Unknown shapes are left unchanged. Used only by the attack injector
// (config_inject).
func configInjectRewrite(body []byte, sysMsg string) []byte {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	switch {
	case func() bool { _, ok := m["system"]; return ok }():
		switch s := m["system"].(type) {
		case string:
			m["system"] = sysMsg + "\n\n" + s
		case []any:
			m["system"] = append([]any{map[string]any{"type": "text", "text": sysMsg}}, s...)
		default:
			m["system"] = sysMsg
		}
	case func() bool { _, ok := m["messages"].([]any); return ok }():
		msgs := m["messages"].([]any)
		m["messages"] = append([]any{map[string]any{"role": "system", "content": sysMsg}}, msgs...)
	case func() bool { _, ok := m["contents"]; return ok }():
		if instr, ok := m["systemInstruction"].(map[string]any); ok {
			if parts, ok := instr["parts"].([]any); ok {
				instr["parts"] = append([]any{map[string]any{"text": sysMsg}}, parts...)
			} else {
				instr["parts"] = []any{map[string]any{"text": sysMsg}}
			}
		} else {
			m["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": sysMsg}}}
		}
	default:
		m["system"] = sysMsg
	}
	if b, err := json.Marshal(m); err == nil {
		return b
	}
	return body
}

type guardRoundTripper struct {
	guard         *Guard
	base          http.RoundTripper
	session       string
	checkResponse bool // when true, also run a post_llm check on responses (WithResponseCheck)
}

// resolveSession prefers a per-request session (set via ContextWithSession,
// e.g. by the proxy from a header) and falls back to this wrap's session for
// a single long-lived SDK client.
func (t *guardRoundTripper) resolveSession(req *http.Request) string {
	if s := sessionFromContext(req.Context()); s != "" {
		return s
	}
	return t.session
}

func (t *guardRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ex := extractorFor(req.URL.Host, req.URL.Path)
	if ex == nil || req.Body == nil {
		return t.base.RoundTrip(req) // not a known LLM call — pass through
	}

	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	// Restore the body so the forwarded request is byte-identical.
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }

	// config_inject: while a simulation is active in attack mode, rewrite the LLM request
	// to insert an adversarial system message before it is checked + forwarded — the same seam the
	// wrap already owns for pre_llm, flipped from observe to mutate.
	if t.guard.simCtl != nil {
		if sysMsg, ok := t.guard.simCtl.InjectLLMSystemMessage(req.URL.Host); ok {
			body = configInjectRewrite(body, sysMsg)
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		}
	}

	// In-band proxy mode: route the ACTUAL call through prism /v1/proxy (prism enforces inline and
	// forwards to the provider), instead of the out-of-band /check + direct forward.
	if t.guard.cfg.ProxyMode {
		return t.proxyForward(req, body)
	}

	session := t.resolveSession(req)

	prompt, model := ex(body)
	_, err = t.guard.Check(req.Context(), CheckRequest{
		RequestID:     "req-" + randHex(),
		SessionID:     session,
		Stage:         StagePreLLM,
		ComponentType: "LLM",
		ComponentName: req.URL.Host,
		MethodName:    "http",
		Content:       Content{Full: prompt},
		Operation:     Operation{Type: "chat.completions", ModelID: model, DestDomain: req.URL.Host},
	})
	if err != nil {
		return nil, err // Deny/*DenyError → the SDK call fails; provider not contacted
	}

	resp, rerr := t.base.RoundTrip(req)
	if rerr != nil || !t.checkResponse || resp == nil {
		return resp, rerr
	}
	return t.inspectResponse(req, resp, session, model)
}

// inspectResponse runs the post_llm check on the model response. Non-streaming responses are
// buffered, checked, and BLOCKED on deny (returns *DenyError, drops the response). Streaming (SSE)
// responses are wrapped so the completion is checked when the stream ends — monitor + record only,
// since already-streamed tokens can't be retracted.
func (t *guardRoundTripper) inspectResponse(req *http.Request, resp *http.Response, session, model string) (*http.Response, error) {
	host := req.URL.Host

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		ctx := req.Context()
		resp.Body = newStreamMonitor(resp.Body, host, func(completion string) {
			if completion != "" {
				_, _ = t.guard.CheckModelResponse(ctx, session, model, completion) // record/audit
			}
		})
		return resp, nil
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	completion := respExtractorFor(host)(body)
	if completion != "" {
		if _, cerr := t.guard.CheckModelResponse(req.Context(), session, model, completion); cerr != nil {
			return nil, cerr // post_llm Deny → drop the response; caller gets *DenyError
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(body)) // restore for the caller
	return resp, nil
}

// proxyForward rewrites an LLM request to route through prism /v1/proxy: the original provider URL
// goes in X-Destination, the body is DID-signed, and prism enforces inline then forwards to the
// provider and streams the response back. The provider's own auth headers pass through unchanged.
// A policy deny surfaces as prism's HTTP error status (the SDK sees it as an API error).
func (t *guardRoundTripper) proxyForward(req *http.Request, body []byte) (*http.Response, error) {
	dest := req.URL.String()
	prismURL, err := url.Parse(strings.TrimRight(t.guard.cfg.APIURL, "/") + "/v1/proxy")
	if err != nil {
		return nil, err
	}
	req.URL = prismURL
	req.Host = prismURL.Host
	req.Header.Set("X-Destination", dest)
	req.Header.Set("X-CompFly-Stage", string(StagePreLLM))
	// Session correlation: the same session id this wrap stamps on its /check
	// telemetry, so prism keys its rows to the real session instead of minting
	// a fresh id per request. NOTE: a client-provided session id shifts prism's
	// execution-mode classification (autonomous → interactive), which feeds
	// policy evaluation — a known, accepted behavior change. An explicit header
	// set by the caller wins.
	if req.Header.Get("X-Session-ID") == "" {
		req.Header.Set("X-Session-ID", t.resolveSession(req))
	}
	if t.guard.signer != nil {
		hdrs, err := t.guard.signer.Sign(body, time.Now())
		if err != nil {
			return nil, err
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
	}
	return t.base.RoundTrip(req)
}

// extractor pulls the latest user message and model from a provider request body.
// Pre-LLM content controls must inspect untrusted user input, not trusted system
// instructions or assistant/tool history.
type extractor func(body []byte) (prompt, model string)

// extractorFor selects the provider extractor for a host+path, or nil if the host isn't a known
// LLM provider (so unrelated traffic on the same client passes through untouched).
func extractorFor(host, path string) extractor {
	switch {
	case strings.Contains(host, "anthropic.com") && strings.Contains(path, "/messages"):
		return extractAnthropic
	case strings.Contains(host, "openai.com") && strings.Contains(path, "/chat/completions"):
		return extractOpenAI
	// Gemini (Google AI Studio / generativelanguage) carries the model in the URL path
	// (/v1beta/models/{model}:generateContent), not the body, so the path is captured here.
	case strings.Contains(host, "generativelanguage.googleapis.com") && strings.Contains(path, "/models/"):
		p := path
		return func(body []byte) (string, string) { return extractGemini(body, p) }
	// Azure OpenAI and OpenAI-compatible gateways also use /chat/completions:
	case strings.Contains(path, "/chat/completions"):
		return extractOpenAI
	default:
		return nil
	}
}

// extractAnthropic reads the latest user message from an Anthropic /v1/messages body.
func extractAnthropic(body []byte) (string, string) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return string(body), ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return contentText(req.Messages[i].Content), req.Model
		}
	}
	return "", req.Model
}

// extractOpenAI reads the latest user message from an OpenAI /v1/chat/completions body.
func extractOpenAI(body []byte) (string, string) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return string(body), ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return contentText(req.Messages[i].Content), req.Model
		}
	}
	return "", req.Model
}

// extractGemini reads the latest user message from a Gemini generateContent/streamGenerateContent
// body ({"contents":[{"role":"user","parts":[{"text":"..."}]}]}); the model comes from the URL path
// since Gemini doesn't carry it in the body.
func extractGemini(body []byte, path string) (string, string) {
	model := geminiModelFromPath(path)
	var req struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if json.Unmarshal(body, &req) != nil {
		return string(body), model
	}
	for i := len(req.Contents) - 1; i >= 0; i-- {
		if req.Contents[i].Role == "user" {
			var b strings.Builder
			for _, p := range req.Contents[i].Parts {
				b.WriteString(p.Text)
			}
			return b.String(), model
		}
	}
	return "", model
}

// geminiModelFromPath pulls the model id out of a Gemini path like
// "/v1beta/models/gemini-1.5-pro:generateContent" -> "gemini-1.5-pro".
func geminiModelFromPath(path string) string {
	i := strings.LastIndex(path, "/models/")
	if i < 0 {
		return ""
	}
	rest := path[i+len("/models/"):]
	if j := strings.IndexByte(rest, ':'); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// contentText extracts text from a message content field that may be a plain string or an array of
// content parts ({type,text} for both Anthropic and OpenAI).
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text + " ")
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// randHex returns 16 hex chars of randomness for request/session ids.
func randHex() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
