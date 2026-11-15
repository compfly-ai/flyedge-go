package langchaingo

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	flyedge "github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/enforce"
	"github.com/tmc/langchaingo/llms/openai"
)

func TestRepeatedGovernedModelInvocationsPreserveHistoryExactlyOnce(t *testing.T) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	var mu sync.Mutex
	var seen [][]message
	var checks []enforce.CheckRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/flyedge/check":
			var check enforce.CheckRequest
			if err := json.NewDecoder(r.Body).Decode(&check); err != nil {
				t.Errorf("decode policy request: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			mu.Lock()
			checks = append(checks, check)
			mu.Unlock()
			fmt.Fprint(w, `{"decision":"allow"}`)
		case "/v1/chat/completions":
			var request struct {
				Messages []message `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Messages) == 0 {
				t.Errorf("invalid model request: messages=%v err=%v", request.Messages, err)
				http.Error(w, "invalid model request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			seen = append(seen, request.Messages)
			mu.Unlock()
			reply := "reply to " + request.Messages[len(request.Messages)-1].Content
			if err := json.NewEncoder(w).Encode(map[string]any{
				"id": "fixture", "object": "chat.completion", "model": "gpt-4o",
				"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": message{"assistant", reply}}},
			}); err != nil {
				t.Errorf("encode reply: %v", err)
			}
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	cloudTelemetry := false
	guard, err := flyedge.New(flyedge.Config{
		APIURL: server.URL, CloudTelemetry: &cloudTelemetry, FailMode: flyedge.FailClosed,
	}, flyedge.WithSimulation(false))
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	client := &http.Client{
		Transport: guard.WrapRoundTripper(server.Client().Transport, flyedge.WithResponseCheck()),
		Timeout:   5 * time.Second,
	}
	model, err := openai.New(openai.WithToken("fixture"), openai.WithModel("gpt-4o"),
		openai.WithBaseURL(server.URL+"/v1"), openai.WithHTTPClient(client))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(flyedge.ContextWithSession(context.Background(), "conversation"), 30*time.Second)
	defer cancel()
	// Reuse the same model client and real Go Guard across all stateless handler calls.
	invoke := func(request flyedge.InvocationRequest) (string, error) {
		messages, err := Messages(request)
		if err != nil {
			return "", err
		}
		response, err := model.GenerateContent(ctx, messages)
		if err != nil {
			return "", err
		}
		if len(response.Choices) != 1 {
			return "", fmt.Errorf("got %d choices", len(response.Choices))
		}
		return response.Choices[0].Content, nil
	}
	for _, mode := range []string{"nil", "empty", "seeded"} {
		t.Run(mode, func(t *testing.T) {
			request := flyedge.InvocationRequest{}
			want := []message{}
			history := []flyedge.InvocationMessage{}
			if mode == "seeded" {
				history = []flyedge.InvocationMessage{{Role: "user", Content: "seed question"}, {Role: "assistant", Content: "seed answer"}}
				want = append(want, message{"user", "seed question"}, message{"assistant", "seed answer"})
			}
			if mode != "nil" {
				request.History = &history
			}
			for turn := 0; turn < 5; turn++ {
				prompt := fmt.Sprintf("question %d", turn)
				request.Messages = append(request.Messages, flyedge.InvocationMessage{Role: "user", Content: prompt})
				want = append(want, message{"user", prompt})
				before, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				var reply string
				for retry := 0; retry < 2; retry++ {
					reply, err = invoke(request)
					if err != nil {
						t.Fatalf("turn %d retry %d: %v", turn, retry, err)
					}
					if reply != "reply to "+prompt {
						t.Fatalf("unexpected reply: %q", reply)
					}
					mu.Lock()
					got := seen[len(seen)-1]
					mu.Unlock()
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("turn %d retry %d: model received %#v, want %#v", turn, retry, got, want)
					}
					after, err := json.Marshal(request)
					if err != nil {
						t.Fatal(err)
					}
					if string(after) != string(before) {
						t.Fatalf("turn %d retry %d: request mutated", turn, retry)
					}
				}
				// Only the new reply enters live messages; seed history stays separate.
				request.Messages = append(request.Messages, flyedge.InvocationMessage{Role: "assistant", Content: reply})
				want = append(want, message{"assistant", reply})
			}
		})
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 30 {
		t.Fatalf("model calls = %d, want 30", len(seen))
	}
	if len(checks) != 60 {
		t.Fatalf("policy checks = %d, want 60", len(checks))
	}
	for i, check := range checks {
		stage := flyedge.StagePreLLM
		if i%2 == 1 {
			stage = flyedge.StagePostLLM
		}
		if check.Stage != stage || check.SessionID != "conversation" {
			t.Errorf("check %d: stage=%s session=%s", i, check.Stage, check.SessionID)
		}
	}
}
