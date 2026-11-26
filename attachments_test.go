package flyedge

import (
	"context"
	"encoding/json"
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

const mediaBody = `{"model":"gpt-4o-mini","messages":[{"role":"user","content":[{"type":"text","text":"Read this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,c3ludGhldGlj"}}]}]}`

func TestMediaTransportSendsAttachmentsAndPreservesProviderInput(t *testing.T) {
	rec := &mediaChecker{decision: Decision{Action: ActionAllow, Detail: map[string]any{"intent_metadata_json": `{"nontext_status":"decoded"}`}}}
	g := mediaGuard(t, rec, FailOpen)
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
	if len(payload.Attachments) == 0 {
		t.Fatal("inline media not sent")
	}
	if strings.Contains(payload.Full, "c3ludGhldGlj") {
		t.Fatal("binary leaked into policy text")
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
			dec, err := g.Check(context.Background(), CheckRequest{Content: Content{Full: "caption", Attachments: []Attachment{{ID: "a", MediaType: "image/png", DataBase64: "c3ludGhldGlj"}}}})
			wantDeny := reason == "deny" || (reason != "decoded" && mode == FailClosed)
			if (err != nil) != wantDeny || (dec.Action == ActionDeny) != wantDeny {
				t.Fatalf("%s/%s unexpected verdict: %+v %v", mode, reason, dec, err)
			}
			if reason == "decoded" && dec.NontextStatus != "decoded" {
				t.Fatal("decoder acknowledgment missing")
			}
		}
	}
}

func TestMediaFailureNeverRetriesTextOnly(t *testing.T) {
	ack := func(status string) map[string]any {
		return map[string]any{"intent_metadata_json": `{"nontext_status":"` + status + `"}`}
	}
	cases := map[string]struct {
		decision Decision
		err      error
		denyOpen bool // verdict under FailOpen; FailClosed denies every case but skipped_no_controls
	}{
		"transport":           {decision: Decision{Action: ActionAllow}, err: errors.New("503 unavailable")},
		"unacknowledged":      {decision: Decision{Action: ActionAllow, Detail: ack("uninspected")}},
		"uninspected deny":    {decision: Decision{Action: ActionDeny, Reason: "nontext_uninspected"}, denyOpen: true},
		"limit deny":          {decision: Decision{Action: ActionDeny, Reason: "nontext_limit_exceeded"}, denyOpen: true},
		"skipped_no_controls": {decision: Decision{Action: ActionAllow, Detail: ack("skipped_no_controls")}},
	}
	for _, mode := range []FailMode{FailOpen, FailClosed} {
		for name, c := range cases {
			rec := &mediaChecker{decision: c.decision, err: c.err}
			g := mediaGuard(t, rec, mode)
			base := &mediaProvider{}
			req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", strings.NewReader(mediaBody))
			resp, err := g.WrapRoundTripper(base).RoundTrip(req)
			if resp != nil {
				resp.Body.Close()
			}
			wantDeny := c.denyOpen || (mode == FailClosed && name != "skipped_no_controls")
			var denied *DenyError
			if errors.As(err, &denied) != wantDeny || (base.calls == 0) != wantDeny {
				t.Errorf("mode=%s %s: provider calls=%d err=%v, want deny=%v", mode, name, base.calls, err, wantDeny)
			}
			if len(rec.requests) != 1 || len(rec.requests[0].Content.Attachments) != 1 {
				t.Errorf("mode=%s %s: want one check carrying the attachment, got %d checks", mode, name, len(rec.requests))
			}
		}
	}
	rec := &mediaChecker{decision: Decision{Action: ActionAllow, Detail: ack("skipped_no_controls")}}
	dec, err := mediaGuard(t, rec, FailClosed).Check(context.Background(), CheckRequest{Content: Content{Full: "caption", Attachments: []Attachment{{ID: "a", MediaType: "image/png", DataBase64: "c3ludGhldGlj"}}}})
	if err != nil || dec.Action != ActionAllow || dec.NontextStatus != "skipped_no_controls" {
		t.Fatalf("skipped_no_controls must allow and keep its status: %+v %v", dec, err)
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

func zeroPDFBase64(size int) string {
	return strings.Repeat("A", size/3*4) + []string{"", "AA==", "AAA="}[size%3]
}

func pdfAttachments(t *testing.T, sizes ...int) []Attachment {
	t.Helper()
	var blocks []any
	for _, size := range sizes {
		blocks = append(blocks, map[string]any{"type": "input_file", "file_data": "data:application/pdf;base64," + zeroPDFBase64(size)})
	}
	items := ExtractAttachments([]any{map[string]any{"role": "user", "content": blocks}})
	body, _ := json.Marshal(CheckRequest{Content: Content{Full: "read these", Attachments: items}})
	if len(body) >= 40<<20 {
		t.Fatalf("check body %d bytes, over Prism's 40 MiB limit", len(body))
	}
	return items
}

func TestFilesUpTo25MiBReachPEAsBytes(t *testing.T) {
	for _, sizes := range [][]int{{12 << 20}, {25 << 20}, {2621440, 2621440, 2621440, 2621440, 2621440, 2621440, 2621440, 2621440, 2621440, 2621440}} {
		items := pdfAttachments(t, sizes...)
		if len(items) != len(sizes) {
			t.Fatalf("%d attachments for %d files", len(items), len(sizes))
		}
		for i, item := range items {
			if item.DataBase64 != zeroPDFBase64(sizes[i]) || item.Oversize != nil {
				t.Fatalf("file %d of %d bytes was not sent as bytes", i, sizes[i])
			}
		}
	}
}

func TestFilesPastTheWireBudgetGoAsFileMarkersPEAccepts(t *testing.T) {
	marker := func(item Attachment) string {
		raw, _ := json.Marshal(item)
		return string(raw)
	}
	items := pdfAttachments(t, 25<<20, 10<<20, 3)
	if len(items) != 3 || items[0].DataBase64 != zeroPDFBase64(25<<20) || items[2].DataBase64 != "AAAA" {
		t.Fatal("the files that fit must keep their bytes")
	}
	if got := marker(items[1]); got != `{"id":"attachment-2","media_type":"application/pdf","data_base64":"","oversize":{"observed_bytes":10485760,"scope":"file"}}` {
		t.Fatal(got)
	}
	items = pdfAttachments(t, 30<<20)
	if got := marker(items[0]); len(items) != 1 || got != `{"id":"attachment-1","media_type":"application/pdf","data_base64":"","oversize":{"observed_bytes":31457280,"scope":"file"}}` {
		t.Fatal(got)
	}
	sizes := make([]int, maxAttachments+1)
	for i := range sizes {
		sizes[i] = 3
	}
	items = pdfAttachments(t, sizes...)
	// PE's validator requires exactly this shape for a count marker.
	if got := marker(items[0]); len(items) != 1 || got != `{"id":"attachment-count","media_type":"unresolved","data_base64":"","oversize":{"limit_attachments":10,"observed_attachments":11,"scope":"attachment_count"}}` {
		t.Fatal(got)
	}
}

func TestToolMediaSentAsAttachmentNotText(t *testing.T) {
	rec := &mediaChecker{decision: Decision{Action: ActionAllow}}
	g := mediaGuard(t, rec, FailOpen)
	input := map[string]any{"image": map[string]any{"type": "image", "source": map[string]any{"media_type": "image/png", "data": "c3ludGhldGlj"}}, "query": "summarize"}
	_, err := g.CheckToolCall(context.Background(), "test", "example", input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rec.requests[0].Content.Full, "c3ludGhldGlj") || strings.Contains(rec.requests[0].Operation.ToolArgsJSON, "c3ludGhldGlj") || len(rec.requests[0].Content.Attachments) != 1 {
		t.Fatal("tool media must leave the text view and travel as one attachment")
	}
	if input["image"].(map[string]any)["source"].(map[string]any)["data"] != "c3ludGhldGlj" {
		t.Fatal("original input mutated")
	}
}

func TestSerializedToolMediaDoesNotEnterAuditText(t *testing.T) {
	value := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"c3ludGhldGlj"}}`
	rec := &mediaChecker{decision: Decision{Action: ActionAllow, Detail: map[string]any{"intent_metadata_json": `{"nontext_status":"decoded"}`}}}
	g := mediaGuard(t, rec, FailOpen)
	_, err := g.CheckToolCall(context.Background(), "fixture", "image_tool", value)
	if err != nil {
		t.Fatal(err)
	}
	request := rec.requests[len(rec.requests)-1]
	if strings.Contains(request.Content.Full, "c3ludGhldGlj") || strings.Contains(request.Operation.ToolArgsJSON, "c3ludGhldGlj") {
		t.Fatal("encoded source entered text")
	}
	if len(request.Content.Attachments) == 0 {
		t.Fatal("serialized tool media not sent")
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

func TestToolDocumentTextStaysVisibleToPolicy(t *testing.T) {
	rec := &mediaChecker{decision: Decision{Action: ActionAllow, Detail: map[string]any{"intent_metadata_json": `{"nontext_status":"decoded"}`}}}
	g := mediaGuard(t, rec, FailOpen)
	input := map[string]any{
		"plain":  map[string]any{"type": "document", "text": "sensitive content"},
		"source": map[string]any{"type": "document", "source": map[string]any{"type": "text", "media_type": "text/plain", "data": "quarterly figures"}},
		"image":  map[string]any{"type": "image", "caption": "scanned form", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "c3ludGhldGlj"}},
	}
	if _, err := g.CheckToolCall(context.Background(), "s", "files", input); err != nil {
		t.Fatal(err)
	}
	r := rec.requests[0]
	for _, s := range []string{r.Content.Full, r.Operation.ToolArgsJSON} {
		for _, want := range []string{"sensitive content", "quarterly figures", "scanned form"} {
			if !strings.Contains(s, want) {
				t.Fatalf("text %q hidden from policy: %s", want, s)
			}
		}
		if strings.Contains(s, "c3ludGhldGlj") {
			t.Fatalf("image bytes entered policy text")
		}
	}
	if len(r.Content.Attachments) != 1 {
		t.Fatalf("attachments=%d", len(r.Content.Attachments))
	}
}

func TestTypedToolInputMediaInspected(t *testing.T) {
	type source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	}
	type block struct {
		Type   string `json:"type"`
		Source source `json:"source"`
	}
	type args struct {
		Query  string  `json:"query"`
		Blocks []block `json:"blocks"`
		Count  int64   `json:"count"`
	}
	in := args{Query: "summarize", Blocks: []block{{Type: "image", Source: source{"base64", "image/png", "c3ludGhldGlj"}}}, Count: 9007199254740993}
	for _, input := range []any{
		in,
		map[string]any{"query": in.Query, "blocks": in.Blocks, "count": in.Count},
		[]any{in.Query, in.Blocks, in.Count},
		map[string]any{"nested": []any{map[string][]args{"arguments": {in}}}},
	} {
		original, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		rec := &mediaChecker{decision: Decision{Action: ActionAllow, Detail: map[string]any{"intent_metadata_json": `{"nontext_status":"decoded"}`}}}
		g := mediaGuard(t, rec, FailOpen)
		if _, err := g.CheckToolCall(context.Background(), "s", "files", input); err != nil {
			t.Fatal(err)
		}
		r := rec.requests[0]
		if strings.Contains(r.Content.Full, "c3ludGhldGlj") || strings.Contains(r.Operation.ToolArgsJSON, "c3ludGhldGlj") {
			t.Fatalf("typed input bytes entered policy text")
		}
		if !strings.Contains(r.Content.Full, "summarize") || !strings.Contains(r.Content.Full, "9007199254740993") {
			t.Fatalf("typed input text changed: %s", r.Content.Full)
		}
		if len(r.Content.Attachments) != 1 {
			t.Fatalf("attachments=%d", len(r.Content.Attachments))
		}
		after, err := json.Marshal(input)
		if err != nil || string(after) != string(original) {
			t.Fatal("original tool input mutated")
		}
	}
}

func TestMediaAcknowledgmentAcceptsNoControlsSkip(t *testing.T) {
	for status, want := range map[string]bool{"decoded": true, "skipped_no_controls": true, "uninspected": false, "skipped_disabled": false, "skipped_oversize": true} {
		dec := Decision{Action: ActionAllow, Detail: map[string]any{"intent_metadata_json": `{"nontext_status":"` + status + `"}`}}
		if _, ok := mediaAcknowledgment(dec); ok != want {
			t.Errorf("%s acknowledged=%v, want %v", status, ok, want)
		}
	}
}

func TestWithoutAttachmentsKeepsTextAndSendsNoMedia(t *testing.T) {
	value := map[string]any{"caption": "chart", "image": map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,c3ludGhldGlj"}}}
	full := ""
	for _, without := range []bool{false, true} {
		rec := &mediaChecker{decision: Decision{Action: ActionAllow, Detail: map[string]any{"intent_metadata_json": `{"nontext_status":"decoded"}`}}}
		opts := []Option{WithSigner(mediaSigner{}), WithEnforcer(rec)}
		if without {
			opts = append(opts, WithoutAttachments())
		}
		g, err := New(Config{FailMode: FailClosed}, opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { g.Close() })
		if _, err := g.CheckToolResponse(context.Background(), "s", "render", value); err != nil {
			t.Fatal(err)
		}
		sent := rec.requests[len(rec.requests)-1].Content
		if got := len(sent.Attachments); got != map[bool]int{false: 1, true: 0}[without] {
			t.Fatalf("without=%v: attachments=%d", without, got)
		}
		if without && sent.Full != full {
			t.Fatalf("text view changed:\n%s\n%s", full, sent.Full)
		}
		full = sent.Full
	}
}
