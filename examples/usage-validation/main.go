// usage-validation runs one streamed OpenAI call per Playground turn in dev.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/compfly-ai/flyedge-go"
)

type providerUsage struct {
	Input         int64 `json:"prompt_tokens"`
	Output        int64 `json:"completion_tokens"`
	Total         int64 `json:"total_tokens"`
	PromptDetails struct {
		Cached int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}
type result struct {
	ID, Model, Text string
	Usage           *providerUsage
}

func readStream(r io.Reader) (result, error) {
	var out result
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 1024*1024)
	done := false
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			continue
		}
		var chunk struct {
			ID      string         `json:"id"`
			Model   string         `json:"model"`
			Usage   *providerUsage `json:"usage"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return out, err
		}
		if chunk.ID != "" {
			out.ID = chunk.ID
		}
		if chunk.Model != "" {
			out.Model = chunk.Model
		}
		if chunk.Usage != nil {
			out.Usage = chunk.Usage
		}
		for _, c := range chunk.Choices {
			out.Text += c.Delta.Content
		}
	}
	if err := s.Err(); err != nil {
		return out, err
	}
	if !done || out.Usage == nil {
		return out, errors.New("stream missing terminal marker or provider usage")
	}
	if out.Usage.Input+out.Usage.Output != out.Usage.Total {
		return out, errors.New("inconsistent provider totals")
	}
	return out, nil
}
func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return errors.New("OPENAI_API_KEY is required")
	}
	cfg := flyedge.LoadEnv()
	if cfg.APIURL != "https://prism.q.compfly.ai" {
		return errors.New("this validation agent requires dev Prism")
	}
	cfg.FailMode = flyedge.FailClosed
	g, err := flyedge.New(cfg, flyedge.WithHeartbeat(time.Second), flyedge.WithCloudTelemetry(time.Second))
	if err != nil {
		return err
	}
	defer g.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := g.Connect(ctx, flyedge.ManifestInfo{Framework: "go-pr52-usage-validation", Environment: "dev", Models: []string{"gpt-5-mini"}}); err != nil {
		return err
	}
	client := &http.Client{Transport: g.WrapRoundTripper(http.DefaultTransport), Timeout: 60 * time.Second}
	c, err := g.ServeInvocations(ctx, flyedge.InvocationOptions{Name: "Go streamed usage validation", Environment: "dev", Purposes: []flyedge.InvocationPurpose{flyedge.PurposePlayground}}, func(ctx context.Context, r flyedge.InvocationRequest, w flyedge.InvocationWriter) error {
		ctx = flyedge.ContextWithSession(ctx, r.SessionID)
		prompt := ""
		for _, m := range r.Messages {
			if m.Role == "user" {
				prompt = m.Content
			}
		}
		// Deliberately omit stream_options: PR52 must request provider usage.
		body, _ := json.Marshal(map[string]any{"model": "gpt-5-mini", "stream": true, "max_completion_tokens": 256, "messages": []map[string]string{{"role": "user", "content": prompt}}})
		req, err := http.NewRequestWithContext(ctx, "POST", "https://api.openai.com/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		started := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("provider HTTP %d", resp.StatusCode)
		}
		result, err := readStream(resp.Body)
		if err != nil {
			return err
		}
		u := result.Usage
		streamed := true
		// This emits telemetry only. The wrapped transport independently builds the
		// invocation usage envelope; do not also call RecordInvocationUsage.
		traceID := ""
		parts := strings.Split(r.Traceparent, "-")
		if len(parts) == 4 {
			traceID = parts[1]
		}
		g.RecordLLMCallDetail(flyedge.LLMCall{SessionID: r.SessionID, RequestID: result.ID, Model: result.Model, Provider: "openai", InputTokens: u.Input - u.PromptDetails.Cached, CacheReadTokens: u.PromptDetails.Cached, OutputTokens: u.Output, Streamed: &streamed, LatencyMS: float64(time.Since(started).Milliseconds()), RequestFull: prompt, ResponseFull: result.Text, TraceID: traceID})
		evidence := map[string]any{"event": "provider.usage", "invocation": r.ID, "session": r.SessionID, "provider_request_id": resp.Header.Get("x-request-id"), "completion_id": result.ID, "model": result.Model, "provider_calls": 1, "usage": u}
		raw, _ := json.Marshal(evidence)
		log.Print(string(raw))
		return w.TextDelta(ctx, result.Text)
	})
	if err != nil {
		return err
	}
	defer c.Close()
	log.Printf("connection.ready id=%s environment=dev", c.ID)
	return c.Wait()
}
