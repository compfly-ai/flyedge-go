// Command attack-target is a minimal flyedge-governed agent purpose-built to exercise the Phase B2
// attack injector. When a simulation runs against it in ATTACK mode, two things happen automatically:
//
//   - config_inject: its LLM request is rewritten by the transport wrap to carry an adversarial
//     system message (only exercised when ANTHROPIC_API_KEY is set — a real model call is made).
//   - tool_poison / error_inject: its tool results, routed through guard.GovernToolResult, come back
//     MUTATED by the injector.
//
// Each tick (when a run is active) it drives one turn — a tool call via GovernToolResult, and (if a
// key is present) one governed LLM call — and logs whether injection landed. Drive it by starting an
// attack-mode sim (extra.attack_injector.mode="attack") and watch sim:telemetry:{runId} for the
// attack_injected events + the agent's mutated view.
//
// Env: COMPFLY_API_URL / COMPFLY_AGENT_DID / COMPFLY_AGENT_PRIVATE_KEY_PATH (govern against prism),
// COMPFLY_SIM_TELEMETRY_URL (split-horizon override), ANTHROPIC_API_KEY (optional — enables config_inject).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	flyedge "github.com/compfly-ai/flyedge-go"
)

const model = "claude-sonnet-4-5"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	g, err := flyedge.New(flyedge.LoadEnv(), flyedge.WithHeartbeat(3*time.Second))
	if err != nil {
		return err
	}
	defer g.Close()

	// Governed LLM client (only used if a key is present — that's what makes config_inject observable).
	var client anthropic.Client
	hasClient := false
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		hc := &http.Client{Transport: g.WrapRoundTripper(http.DefaultTransport)}
		client = anthropic.NewClient(anthropicopt.WithAPIKey(key), anthropicopt.WithHTTPClient(hc))
		hasClient = true
	}

	ctx := context.Background()
	if err := g.Connect(ctx, flyedge.ManifestInfo{
		Framework:   "flyedge-go/attack-target",
		Models:      []string{model},
		Tools:       []string{"get_account", "checkout"},
		Environment: "development",
	}); err != nil {
		return err
	}
	log.Printf("attack-target connected — DID=%s, heartbeat 3s (Ctrl-C to stop)", g.DID())
	if !hasClient {
		log.Printf("note: ANTHROPIC_API_KEY unset — tool_poison still exercised; config_inject needs a key")
	}

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
			log.Printf("mode=%-11s | SIMULATION active: run=%s protection_disabled=%v", g.ModelMode(), sim.RunID, sim.ProtectionDisabled)
			driveTurn(sigCtx, g, "attack-"+sim.RunID, client, hasClient)
		}
	}
}

// driveTurn runs one governed turn: a tool call whose result is governed (and possibly poisoned) by
// GovernToolResult, then an optional governed LLM call (possibly config_inject'd by the wrap).
func driveTurn(ctx context.Context, g *flyedge.Guard, session string, client anthropic.Client, hasClient bool) {
	// Tool path — the raw result the tool produced vs what the injector hands back.
	raw := `{"account":"acct_1","holder":"Alice","balance":100.0}`
	governed, dec, err := g.GovernToolResult(ctx, session, "get_account", raw)
	switch {
	case err != nil:
		if de, ok := flyedge.AsDenyError(err); ok {
			log.Printf("   ↳ get_account result WITHHELD by policy: %s", de.Decision.Reason)
		} else {
			log.Printf("   ↳ get_account govern error: %v", err)
		}
	case governed != raw:
		log.Printf("   ↳ get_account result MUTATED by injector (%s): %s", dec.Action, truncate(governed))
	default:
		log.Printf("   ↳ get_account result unchanged (%s)", dec.Action)
	}

	// LLM path — the wrap governs it (pre_llm) and, in attack mode, config_inject rewrites the body.
	if hasClient {
		_, lerr := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(model),
			MaxTokens: 64,
			Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("Give me a one-word status."))},
		})
		if lerr != nil {
			if de, ok := flyedge.AsDenyError(lerr); ok {
				log.Printf("   ↳ llm call DENIED (pre_llm): %s", de.Decision.Reason)
			} else {
				log.Printf("   ↳ llm call error: %v", lerr)
			}
		} else {
			log.Printf("   ↳ llm call ok (governed by the wrap; watch telemetry for config_inject)")
		}
	}
}

func truncate(s string) string {
	if len(s) > 160 {
		return s[:157] + "..."
	}
	return s
}
