// Command sim-agent exercises the flyedge-go config heartbeat poller (Phase A of the simulation
// work). It connects to the gateway, then prints the agent's live model_mode + simulation state on
// each heartbeat — so you can WATCH the poller react to a mode flip or to a simulation started
// against this agent (e.g. a hand-driven PUT /internal/v1/agents/{slug}/simulation, or an
// agent-eval run). No model call is made; the point is the poller, which runs independently once
// Connect succeeds.
//
// Env:
//   COMPFLY_API_URL                 prism base (e.g. http://localhost:8080)
//   COMPFLY_AGENT_DID               the agent's DID (MCP-minted)
//   COMPFLY_AGENT_PRIVATE_KEY_PATH  Ed25519 PEM
package main

import (
	"context"
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
			if sim := g.SimulationConfig(); sim != nil {
				log.Printf("mode=%-11s | SIMULATION active: run=%s protection_disabled=%v middlewares=%v",
					g.ModelMode(), sim.RunID, sim.ProtectionDisabled, sim.Middlewares)
			} else {
				log.Printf("mode=%-11s | no simulation", g.ModelMode())
			}
		}
	}
}
