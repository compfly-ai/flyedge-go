package flyedge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type mediaSigner struct{}

func (mediaSigner) DID() string                                       { return "did:test:media" }
func (mediaSigner) KeyID() string                                     { return "test" }
func (mediaSigner) Sign([]byte, time.Time) (map[string]string, error) { return nil, nil }

type mediaChecker struct {
	requests []CheckRequest
	decision Decision
	err      error
}

func (m *mediaChecker) Check(_ context.Context, r CheckRequest) (Decision, error) {
	m.requests = append(m.requests, r)
	return m.decision, m.err
}

type mediaProvider struct {
	body  string
	calls int
}

func (m *mediaProvider) RoundTrip(r *http.Request) (*http.Response, error) {
	b, _ := io.ReadAll(r.Body)
	m.body = string(b)
	m.calls++
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
}

func mediaGuard(t *testing.T, checker *mediaChecker, mode FailMode) *Guard {
	t.Helper()
	g, err := New(Config{FailMode: mode}, WithSigner(mediaSigner{}), WithEnforcer(checker))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}
func allowMedia(g *Guard) {
	g.applyConfig(flyedgeConfigResponse{AgentSlug: "support", NamespaceID: "ns", AgentSettings: map[string]any{"nontext_payload": "allowed"}})
}

func TestMediaPermissionDefaultsRevocationAndExpiry(t *testing.T) {
	g := mediaGuard(t, &mediaChecker{}, FailOpen)
	if g.NontextPayloadAllowed() {
		t.Fatal("unset permission allowed")
	}
	for _, v := range []any{nil, "", "skipped", "denied", true, "ALLOWED", map[string]any{}} {
		allowMedia(g)
		g.applyConfig(flyedgeConfigResponse{AgentSlug: "support", NamespaceID: "ns", AgentSettings: map[string]any{"nontext_payload": v}})
		if g.NontextPayloadAllowed() {
			t.Fatalf("invalid permission enabled: %v", v)
		}
	}
	allowMedia(g)
	if !g.NontextPayloadAllowed() {
		t.Fatal("explicit permission not applied")
	}
	g.pollMu.Lock()
	g.nontextExpires = time.Now().Add(-time.Second)
	g.pollMu.Unlock()
	if g.NontextPayloadAllowed() {
		t.Fatal("expired permission retained")
	}
	allowMedia(g)
	g.applyConfig(flyedgeConfigResponse{})
	if g.NontextPayloadAllowed() {
		t.Fatal("omitted settings did not revoke")
	}
}

func TestWithAttachmentsSendsWithoutAgentSetting(t *testing.T) {
	rec := &mediaChecker{decision: Decision{Action: ActionAllow, Detail: map[string]any{"intent_metadata_json": `{"nontext_status":"decoded"}`}}}
	g, err := New(Config{}, WithSigner(mediaSigner{}), WithEnforcer(rec), WithAttachments())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	g.applyConfig(flyedgeConfigResponse{AgentSlug: "support", NamespaceID: "ns", AgentSettings: map[string]any{"nontext_payload": "skipped"}})
	dec, err := g.Check(context.Background(), CheckRequest{Content: Content{Full: "caption", Attachments: []Attachment{{ID: "a", MediaType: "image/png", DataBase64: "c3ludGhldGlj"}}}})
	if err != nil || len(rec.requests) != 1 || len(rec.requests[0].Content.Attachments) != 1 || dec.NontextStatus != "decoded" {
		t.Fatalf("WithAttachments must send attachments without the agent setting: checks=%d dec=%+v err=%v", len(rec.requests), dec, err)
	}
}

const mediaBody = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":[{"type":"text","text":"Read this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,c3ludGhldGlj"}}]}]}`

func TestMediaTransportGatedAndProviderInputPreserved(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		rec := &mediaChecker{decision: Decision{Action: ActionAllow, Detail: map[string]any{"intent_metadata_json": `{"nontext_status":"decoded"}`}}}
		g := mediaGuard(t, rec, FailOpen)
		if enabled {
			allowMedia(g)
		}
		base := &mediaProvider{}
		req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", strings.NewReader(mediaBody))
		_, err := g.WrapRoundTripper(base).RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if base.body != mediaBody {
			t.Fatal("provider body was changed")
		}
		if len(rec.requests) != 1 {
			t.Fatalf("checks=%d", len(rec.requests))
		}
		payload := rec.requests[0].Content
		if (len(payload.Attachments) > 0) != enabled {
			t.Fatal("permission did not gate media")
		}
		if strings.Contains(payload.Full, "c3ludGhldGlj") {
			t.Fatal("binary leaked into policy text")
		}
	}
}

func TestMediaAvailabilityAndExplicitDeny(t *testing.T) {
	for _, mode := range []FailMode{FailOpen, FailClosed} {
		for _, reason := range []string{"transport", "unconfirmed", "deny", "decoded"} {
			rec := &mediaChecker{decision: Decision{Action: ActionAllow}}
			switch reason {
			case "transport":
				rec.err = errors.New("503 unavailable")
			case "deny":
				rec.decision = Decision{Action: ActionDeny, Reason: "fixture-deny"}
			case "decoded":
				rec.decision.Detail = map[string]any{"intent_metadata_json": `{"nontext_status":"decoded"}`}
			}
			g := mediaGuard(t, rec, mode)
			allowMedia(g)
			dec, err := g.Check(context.Background(), CheckRequest{Content: Content{Full: "caption", Attachments: []Attachment{{ID: "a", MediaType: "image/png", DataBase64: "c3ludGhldGlj"}}}})
			wantDeny := reason == "deny"
			if (err != nil) != wantDeny || (dec.Action == ActionDeny) != wantDeny {
				t.Fatalf("%s/%s unexpected verdict: %+v %v", mode, reason, dec, err)
			}
			if reason == "decoded" && dec.NontextStatus != "decoded" {
				t.Fatal("decoder acknowledgment missing")
			}
		}
	}
}

func TestMediaShapeLimitsAndNoLocalResolution(t *testing.T) {
	items := ExtractAttachments(map[string]any{"input": []any{map[string]any{"type": "input_file", "file_data": "data:application/pdf;base64,cGRm"}}})
	if len(items) != 1 || items[0].MediaType != "application/pdf" {
		t.Fatal(items)
	}
	remote := ExtractAttachments(map[string]any{"type": "image_url", "image_url": "https://example.invalid/a.png"})
	if len(remote) != 1 || remote[0].MediaType != "unresolved" || remote[0].DataBase64 != "" {
		t.Fatal("reference resolved")
	}
	big := ExtractAttachments(map[string]any{"type": "input_file", "file_data": "data:application/pdf;base64," + strings.Repeat("A", mediaTransportLimit)})
	if len(big) != 1 || big[0].DataBase64 != "" || big[0].Oversize == nil {
		t.Fatal("transport limit did not strip binary")
	}
	if len(ExtractAttachments(`base64 text c3ludGhldGlj`)) != 0 {
		t.Fatal("ordinary text decoded")
	}
}

func TestToolMediaTextSanitizedWhenPermissionOff(t *testing.T) {
	rec := &mediaChecker{decision: Decision{Action: ActionAllow}}
	g := mediaGuard(t, rec, FailOpen)
	input := map[string]any{"image": map[string]any{"type": "image", "source": map[string]any{"media_type": "image/png", "data": "c3ludGhldGlj"}}, "query": "summarize"}
	_, err := g.CheckToolCall(context.Background(), "test", "example", input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.requests[0].Content.Full, "c3ludGhldGlj") || strings.Contains(rec.requests[0].Operation.ToolArgsJSON, "c3ludGhldGlj") || len(rec.requests[0].Content.Attachments) > 0 {
		t.Fatal("disabled tool media forwarded")
	}
	if input["image"].(map[string]any)["source"].(map[string]any)["data"] != "c3ludGhldGlj" {
		t.Fatal("original input mutated")
	}
}

func TestSerializedToolMediaDoesNotEnterAuditText(t *testing.T) {
	value := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"c3ludGhldGlj"}}`
	rec := &mediaChecker{decision: Decision{Action: ActionAllow, Detail: map[string]any{"intent_metadata_json": `{"nontext_status":"decoded"}`}}}
	g := mediaGuard(t, rec, FailOpen)
	for _, allowed := range []bool{false, true} {
		if allowed {
			allowMedia(g)
		}
		_, err := g.CheckToolCall(context.Background(), "fixture", "image_tool", value)
		if err != nil {
			t.Fatal(err)
		}
		request := rec.requests[len(rec.requests)-1]
		if strings.Contains(request.Content.Full, "c3ludGhldGlj") || strings.Contains(request.Operation.ToolArgsJSON, "c3ludGhldGlj") {
			t.Fatal("encoded source entered text")
		}
		if (len(request.Content.Attachments) > 0) != allowed {
			t.Fatal("serialized tool bytes not gated")
		}
	}
}

func TestPlainTextEncodedInputKeepsExistingRoute(t *testing.T) {
	rec := &mediaChecker{decision: Decision{Action: ActionAllow}}
	g := mediaGuard(t, rec, FailOpen)
	text := "Inspect this encoded text: c3ludGhldGlj data:image/png;base64,c3ludGhldGlj"
	_, err := g.Check(context.Background(), CheckRequest{Content: Content{Full: text}})
	if err != nil {
		t.Fatal(err)
	}
	if rec.requests[0].Content.Full != text || len(rec.requests[0].Content.Attachments) != 0 {
		t.Fatal("ordinary text route changed")
	}
}
