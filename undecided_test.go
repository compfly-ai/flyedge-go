package flyedge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A 200 without a verdict this client knows is not an allow: it follows the fail mode, even when it
// carries an accepted media acknowledgement. Matches the Rust client.
func TestUndecidedVerdictFollowsFailMode(t *testing.T) {
	for _, verdict := range []string{`"indeterminate"`, `""`, `"escalate"`} {
		for _, ack := range []string{"decoded", "skipped_no_controls", "skipped_oversize"} {
			for _, mode := range []FailMode{FailOpen, FailClosed} {
				body := `{"decision":` + verdict + `,"reason":"","request_id":"req-u","detail":{"intent_metadata_json":"{\"nontext_status\":\"` + ack + `\"}"}}`
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
				}))
				g, err := New(Config{APIURL: srv.URL, Mode: ModeEnforce, FailMode: mode}, WithSigner(mediaSigner{}))
				if err != nil {
					t.Fatal(err)
				}
				dec, err := g.Check(context.Background(), CheckRequest{Content: Content{Full: "caption",
					Attachments: []Attachment{{ID: "a", MediaType: "image/png", DataBase64: "c3ludGhldGlj"}}}})
				g.Close()
				srv.Close()
				name := verdict + "/" + ack + "/" + string(mode)
				if mode == FailClosed {
					if err == nil || dec.Action != ActionDeny || dec.Reason != "indeterminate" || dec.RequestID != "req-u" {
						t.Fatalf("%s: want an indeterminate deny, got %+v %v", name, dec, err)
					}
				} else if err != nil || dec.Action != ActionAllow || dec.Reason != "fail_open" {
					t.Fatalf("%s: want a fail_open allow, got %+v %v", name, dec, err)
				}
			}
		}
	}
}

// Known verdicts are unchanged.
func TestKnownVerdictsStillDecide(t *testing.T) {
	for verdict, want := range map[string]Action{"allow": ActionAllow, "warn": ActionWarn, "deny": ActionDeny, "block": ActionDeny} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"decision":"` + verdict + `","reason":"r"}`))
		}))
		g, err := New(Config{APIURL: srv.URL, Mode: ModeEnforce, FailMode: FailClosed}, WithSigner(mediaSigner{}))
		if err != nil {
			t.Fatal(err)
		}
		dec, _ := g.Check(context.Background(), CheckRequest{Content: Content{Full: "hello"}})
		g.Close()
		srv.Close()
		if dec.Action != want {
			t.Fatalf("%s: got %+v", verdict, dec)
		}
	}
}
