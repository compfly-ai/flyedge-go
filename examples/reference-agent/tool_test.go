package main

import (
	"context"
	"encoding/json"
	flyedge "github.com/compfly-ai/flyedge-go"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestToolResponseFailureIsNotAnAllowedResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request["stage"] == "tool_call_response" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision":"allow"}`))
	}))
	defer server.Close()
	g, err := flyedge.New(flyedge.Config{APIURL: server.URL, FailMode: flyedge.FailClosed}, flyedge.WithoutCloudTelemetry())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	a := &agent{guard: g}
	_, isErr, step := a.guardedTool(context.Background(), "s", users["alice"], "get_profile", json.RawMessage(`{}`))
	if !isErr || step.allowed {
		t.Fatal("response check failure became an allowed result")
	}
}
