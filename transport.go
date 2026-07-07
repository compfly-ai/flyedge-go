package flyedge

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
func (g *Guard) WrapRoundTripper(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &guardRoundTripper{guard: g, base: base, session: "sess-" + randHex()}
}

type guardRoundTripper struct {
	guard   *Guard
	base    http.RoundTripper
	session string
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

	// Prefer a per-request session (set via ContextWithSession, e.g. by the proxy from a header);
	// fall back to this wrap's session for a single long-lived SDK client.
	session := t.session
	if s := sessionFromContext(req.Context()); s != "" {
		session = s
	}

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
	return t.base.RoundTrip(req)
}

// extractor pulls (prompt, model) from a provider request body.
type extractor func(body []byte) (prompt, model string)

// extractorFor selects the provider extractor for a host+path, or nil if the host isn't a known
// LLM provider (so unrelated traffic on the same client passes through untouched).
func extractorFor(host, path string) extractor {
	switch {
	case strings.Contains(host, "anthropic.com") && strings.Contains(path, "/messages"):
		return extractAnthropic
	case strings.Contains(host, "openai.com") && strings.Contains(path, "/chat/completions"):
		return extractOpenAI
	// Azure OpenAI and OpenAI-compatible gateways also use /chat/completions:
	case strings.Contains(path, "/chat/completions"):
		return extractOpenAI
	default:
		return nil
	}
}

// extractAnthropic reads system + messages text from an Anthropic /v1/messages body.
func extractAnthropic(body []byte) (string, string) {
	var req struct {
		Model    string          `json:"model"`
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return string(body), ""
	}
	var b strings.Builder
	if s := contentText(req.System); s != "" {
		b.WriteString(s + "\n")
	}
	for _, m := range req.Messages {
		if t := contentText(m.Content); t != "" {
			b.WriteString(m.Role + ": " + t + "\n")
		}
	}
	return strings.TrimSpace(b.String()), req.Model
}

// extractOpenAI reads messages text from an OpenAI /v1/chat/completions body.
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
	var b strings.Builder
	for _, m := range req.Messages {
		if t := contentText(m.Content); t != "" {
			b.WriteString(m.Role + ": " + t + "\n")
		}
	}
	return strings.TrimSpace(b.String()), req.Model
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
