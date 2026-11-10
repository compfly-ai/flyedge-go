package flyedge

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/compfly-ai/flyedge-go/simulation"
)

type usageCleanupBlock struct {
	entered, release chan struct{}
	once             sync.Once
}

func (b *usageCleanupBlock) MarshalJSON() ([]byte, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return []byte(`"fixture"`), nil
}

func TestInvocationUsageCompletionIsBoundToEachAdmittedTurn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	completions := make(chan invocationFrame, 5)
	block := &usageCleanupBlock{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block.release) }) }
	defer release()
	returned := make(chan struct{})
	type lateScope struct {
		ctx   context.Context
		usage *invocationUsage
	}
	late := make(chan lateScope, 1)
	var g *Guard
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := json.NewEncoder(w).Encode(map[string]any{"id": strings.Repeat("a", 32), "token": "fixture-token", "expires_at": time.Now().Add(time.Minute)}); err != nil {
				t.Error(err)
			}
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.CloseNow()
		if err := wsjson.Write(ctx, ws, invocationFrame{Type: "ready"}); err != nil {
			t.Error(err)
			return
		}
		defer release()
		for _, id := range []string{"known", "partial", "next", "failed", "cleanup"} {
			req := &InvocationRequest{ID: id, Purpose: PurposeEval, SessionID: "session", Messages: []InvocationMessage{{Role: "user", Content: "fixture"}}, Deadline: time.Now().Add(time.Second)}
			if id == "cleanup" {
				req.RunID = "run"
				req.SessionID = "eval:run:cleanup"
				req.ScenarioPlan = json.RawMessage(`{"id":"cleanup","session_id":"eval:run:cleanup","step_id":"s","overrides":[]}`)
			}
			if err := wsjson.Write(ctx, ws, invocationFrame{Type: "invoke", Request: req}); err != nil {
				t.Error(err)
				return
			}
			if id == "cleanup" {
				<-returned
				s := <-late
				// Cleanup owns the controller lock. Give the handler completion
				// boundary time to close without allowing cleanup to finish.
				until := time.Now().Add(150 * time.Millisecond)
				for time.Now().Before(until) {
					s.usage.mu.Lock()
					closed := s.usage.closed
					s.usage.mu.Unlock()
					if closed {
						break
					}
					time.Sleep(time.Millisecond)
				}
				g.RecordInvocationUsage(s.ctx, "detached", "model", &ModelUsage{TotalTokens: measured(999)})
				s.usage.complete(s.ctx, "pending", &ModelUsage{InputTokens: measured(8), OutputTokens: measured(9)})
				release()
			}
			var frame invocationFrame
			if err := wsjson.Read(ctx, ws, &frame); err != nil {
				t.Error(err)
				return
			}
			completions <- frame
		}
		if err := wsjson.Write(ctx, ws, invocationFrame{Type: "revoked"}); err != nil {
			t.Error(err)
		}
		_, _, _ = ws.Read(ctx)
	}))
	defer broker.Close()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	g, err = New(Config{APIURL: broker.URL, DID: "did:compfly:org:agent", KeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})}, WithoutCloudTelemetry())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	defer release()
	g.simCtl = simulation.New("test")
	allowInjection := true
	var old context.Context
	connection, err := g.ServeInvocations(ctx, InvocationOptions{Environment: "dev", Purposes: []InvocationPurpose{PurposeEval}, AllowSimulationInjection: &allowInjection, AllowInsecureLoopback: true}, func(ctx context.Context, req InvocationRequest, _ InvocationWriter) error {
		if old != nil {
			g.RecordInvocationUsage(old, "late", "model", &ModelUsage{TotalTokens: measured(999)})
		}
		old = ctx
		counts := &ModelUsage{InputTokens: measured(10), CachedInputTokens: measured(3), OutputTokens: measured(2), ReasoningTokens: measured(1)}
		if req.ID == "partial" {
			counts.OutputTokens, counts.ReasoningTokens = nil, nil
		}
		g.RecordInvocationUsage(ctx, "call", "model", counts)
		g.RecordInvocationUsage(ctx, "call", "model", counts)
		if req.ID == "partial" {
			g.RecordInvocationUsage(ctx, "known-later", "model", &ModelUsage{InputTokens: measured(3), OutputTokens: measured(4)})
		}
		(&Guard{}).RecordInvocationUsage(ctx, "judge", "judge", &ModelUsage{TotalTokens: measured(1000)})
		if req.ID == "failed" {
			return errors.New("fixture failure")
		}
		if req.ID == "cleanup" {
			scope := ctx.Value(invocationUsageContextKey{}).(*invocationUsage)
			scope.start("pending", "model")
			go g.simCtl.ObserveScenarioTool(req.SessionID, "component.executed", "fixture", map[string]any{"result": block})
			<-block.entered
			late <- lateScope{ctx, scope}
			defer close(returned)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Wait(); !errors.Is(err, ErrConnectionRevoked) {
		t.Fatal(err)
	}
	var frames []invocationFrame
	for i := 0; i < 5; i++ {
		frame := <-completions
		frames = append(frames, frame)
		if frame.ID == "failed" {
			if frame.Type != "failed" || frame.Error != "fixture failure" || frame.AgentUsage != nil {
				t.Fatalf("failed invocation advertised completed usage: %+v", frame)
			}
			continue
		}
		usage := frame.AgentUsage
		if frame.ID == "cleanup" {
			if frame.Type != "completed" || usage == nil || usage.ModelCalls != 2 || usage.ReportedCalls != 1 || usage.UnknownCalls != 1 || usage.TotalTokens != nil || *usage.InputTokens != 10 || *usage.OutputTokens != 2 {
				t.Fatalf("cleanup admitted detached usage or completed a pending call: %+v", frame)
			}
			continue
		}
		if frame.Type != "completed" || usage == nil || usage.InvocationID != frame.ID || usage.SessionID != "session" {
			t.Fatalf("usage escaped invocation boundary: %+v", frame)
		}
		if frame.ID == "partial" {
			if usage.ModelCalls != 2 || usage.UnknownCalls != 1 || usage.ReportedCalls != 1 || usage.TotalTokens != nil || *usage.InputTokens != 13 || *usage.OutputTokens != 4 {
				t.Fatal("partial usage became complete")
			}
		} else if usage.ModelCalls != 1 || *usage.InputTokens != 10 || usage.UnknownCalls != 0 || usage.TotalTokens == nil || *usage.TotalTokens != 12 {
			t.Fatal("measured usage disappeared")
		}
	}
	if path := os.Getenv("FLYEDGE_GO_USAGE_FIXTURE_OUTPUT"); path != "" {
		data, err := json.MarshalIndent(frames, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
