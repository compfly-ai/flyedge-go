// Command sim-agent makes a flyedge-go agent a live simulation target (Phase B of the simulation
// work). It connects to the gateway and runs the config heartbeat poller; when a simulation is
// started against this agent (a hand-driven PUT /internal/v1/agents/{slug}/simulation, or an
// agent-eval run) the poller surfaces the simulation block, the SDK's simulation controller
// activates, and every Check the agent runs is streamed as a RuntimeEvent over the telemetry
// WebSocket to prism (→ Redis sim:telemetry:{runId} → eval-runner).
//
// So you can SEE the stream, while a run is active the agent emits a small synthetic "turn" each
// tick: a couple of benign tool/model checks plus two deliberately suspicious ones (an external
// URL in tool args, a credential in a tool result) that the behavior_monitor middleware flags.
// When no run is active it just prints the live model_mode + simulation state — no traffic.
//
// Env:
//   COMPFLY_API_URL                 prism base (e.g. http://localhost:8080)
//   COMPFLY_AGENT_DID               the agent's DID (MCP-minted)
//   COMPFLY_AGENT_PRIVATE_KEY_PATH  Ed25519 PEM
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"time"

	flyedge "github.com/compfly-ai/flyedge-go"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	g, err := flyedge.New(flyedge.LoadEnv(),
		// Poll fast so mode/simulation changes show up quickly during local testing.
		flyedge.WithHeartbeat(3*time.Second),
		flyedge.WithModeChangeHandler(func(old, cur flyedge.ModelMode) {
			log.Printf("⚙  model_mode changed: %s → %s", old, cur)
		}),
	)
	if err != nil {
		return err
	}
	defer g.Close()

	log.Printf("agent DID: %s", g.DID())
	ctx := context.Background()
	if err := g.Connect(ctx, flyedge.ManifestInfo{
		Framework:   "flyedge-go/sim-agent",
		Models:      []string{"claude-sonnet-4-5"},
		Tools:       []string{"get_weather", "lookup_order"},
		Environment: "development",
	}); err != nil {
		return err
	}
	log.Printf("connected — model_mode=%s, heartbeat every 3s (Ctrl-C to stop)", g.ModelMode())

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-sigCtx.Done():
			log.Println("shutting down")
			return nil
		case <-t.C:
			sim := g.SimulationConfig()
			if sim == nil {
				log.Printf("mode=%-11s | no simulation", g.ModelMode())
				continue
			}
			log.Printf("mode=%-11s | SIMULATION active: run=%s protection_disabled=%v middlewares=%v",
				g.ModelMode(), sim.RunID, sim.ProtectionDisabled, sim.Middlewares)
			// A run is active — emit a turn so RuntimeEvents actually stream to the telemetry WS.
			emitTurn(sigCtx, g, "sim-"+sim.RunID)
		}
	}
}

// emitTurn runs one synthetic agent turn through the Guard's Check seams. While a simulation is
// active each Check is recorded as a RuntimeEvent (with any behavior_monitor flags) and streamed to
// prism. The two suspicious checks exercise the flag detectors: an external URL in tool args and a
// leaked credential in a tool result.
func emitTurn(ctx context.Context, g *flyedge.Guard, session string) {
	steps := []struct {
		what string
		run  func() (flyedge.Decision, error)
	}{
		{"tool_call lookup_order (benign)", func() (flyedge.Decision, error) {
			return g.CheckToolCall(ctx, session, "lookup_order", map[string]any{"order_id": "A-1001"}, "")
		}},
		{"tool_response lookup_order (benign)", func() (flyedge.Decision, error) {
			return g.CheckToolResponse(ctx, session, "lookup_order", map[string]any{"status": "shipped", "eta": "2d"})
		}},
		{"model_response (benign)", func() (flyedge.Decision, error) {
			return g.CheckModelResponse(ctx, session, "claude-sonnet-4-5", "Your order A-1001 has shipped and arrives in 2 days.")
		}},
		{"tool_call fetch → external URL (flagged)", func() (flyedge.Decision, error) {
			return g.CheckToolCall(ctx, session, "fetch", map[string]any{"url": "https://evil.example.com/exfil"}, "evil.example.com")
		}},
		{"tool_response → credential leak (flagged)", func() (flyedge.Decision, error) {
			return g.CheckToolResponse(ctx, session, "fetch", "api_key=supersecretvalue123 token=abcd1234")
		}},
	}
	for _, s := range steps {
		dec, err := s.run()
		switch {
		case err != nil:
			var de *flyedge.DenyError
			if errors.As(err, &de) {
				log.Printf("   ↳ %-42s DENY (%s)", s.what, de.Decision.Reason)
			} else {
				log.Printf("   ↳ %-42s error: %v", s.what, err)
			}
		default:
			log.Printf("   ↳ %-42s %s (%s)", s.what, dec.Action, dec.Reason)
		}
	}
}
