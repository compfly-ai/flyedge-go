package enforce

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConditionalDownloadLimit(t *testing.T) {
	payload := strings.Repeat("x", 2<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == "current" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(payload))
	}))
	defer srv.Close()
	client := NewHTTPEnforcer(srv.URL, nil, time.Second)
	for _, budget := range []int64{int64(len(payload)), int64(len(payload) - 1)} {
		body, _, err := client.GetSignedConditionalWithLimit(context.Background(), "/packs", nil, budget)
		if budget == int64(len(payload)) {
			if err != nil || string(body) != payload {
				t.Fatalf("exact limit: len=%d err=%v", len(body), err)
			}
		} else if err == nil || body != nil {
			t.Fatal("oversized response returned as success")
		}
	}
	if _, _, err := client.GetSignedConditional(context.Background(), "/packs", nil); err == nil {
		t.Fatal("default limit bypassed")
	}
	if body, unchanged, err := client.GetSignedConditionalWithLimit(context.Background(), "/packs", map[string]string{"If-None-Match": "current"}, 3<<20); err != nil || !unchanged || body != nil {
		t.Fatalf("304: %v %v", unchanged, err)
	}
}
