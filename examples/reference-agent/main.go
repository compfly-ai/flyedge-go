// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

// Command reference-agent is the complete flyedge-go integration surface in one runnable
// service — the shape of a production governed agent, distilled from CompFly's internal reference
// agent (a governed multi-tool concierge) with its extra protocol layers stripped away. It wires
// EVERYTHING the SDK offers, in the three shapes a real agent runs in: an interactive chat REPL, a
// scripted one-off (-input), and an OpenAI-compatible HTTP endpoint (-serve) the CompFly playground
// and simulation engine can drive.
//
// The full integration surface:
//
//   - Guard construction from env (flyedge.LoadEnv) — explicit, no globals.
//   - Connect: publish the agent manifest (framework, models, tools) and start the config
//     heartbeat, so the platform sees the agent online and can flip its model mode at runtime
//     (surfaced via WithModeChangeHandler).
//   - Local controls: SyncLocalControls pulls the org's client-evaluable rule set and keeps it
//     current — in-process denials that still work when the gateway is unreachable. Local
//     evaluation only ever ADDS a deny; the gateway stays authoritative for what it allows.
//   - pre_llm + post_llm: ONE governed http.Client (WrapRoundTripper + WithResponseCheck)
//     installed into every model SDK — the same wrap governs both providers here.
//   - tool_call: CheckToolCall gates every tool BEFORE it executes. Denials and kill switches are
//     typed errors, fed back to the model as tool results so the agent adapts instead of crashing.
//   - tool_call_response: GovernToolResult governs a tool's output before it re-enters the model's
//     context — the redaction/injection seam.
//   - On-behalf-of identity: ContextWithPrincipal attributes every governed call in a turn (model
//     round-trips AND tool checks) to the end user the agent is acting for, so one agent identity
//     is governed per-user — a policy can key on obo.scope.plan ("free → deny payments"). The HTTP
//     endpoint selects the principal per request from the X-CompFly-On-Behalf-Of header.
//   - Sessions: ContextWithSession ties a turn's model calls and tool checks together.
//   - A provider/model picker: multi-provider (Anthropic | OpenAI | Gemini) behind one
//     provider-neutral tool loop, with a live model menu fetched from the chosen provider.
//   - The protection report at exit.
//
// Run (see README.md for the full walkthrough):
//
//	export ANTHROPIC_API_KEY=...  # and/or OPENAI_API_KEY / GEMINI_API_KEY
//	export COMPFLY_API_URL=https://prism.p.compfly.ai   # the SDK default when unset
//	export COMPFLY_AGENT_DID=did:compfly:...
//	export COMPFLY_AGENT_PRIVATE_KEY_PATH=/path/to/agent.pem
//	export FLYEDGE_MODE=enforce                     # optional; this example defaults to enforce
//	go run ./reference-agent/                       # interactive chat (prompts provider + model)
//	go run ./reference-agent/ -user bob -input "send $40 to dana@example.com"
//	go run ./reference-agent/ -serve                # OpenAI-compatible endpoint on :8900
//
// OTEL=1 additionally exports each guard decision as a flyedge.check OpenTelemetry span to stdout.
//
// Without COMPFLY_* set the agent still runs: checks fail open (recorded, not enforced) and
// Connect/local-control sync report themselves unavailable — the same graceful degradation a
// production agent needs.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/compfly-ai/flyedge-go"
	"github.com/compfly-ai/flyedge-go/localcontrol"
	feotel "github.com/compfly-ai/flyedge-go/telemetry/otel"
)

// localControlPollInterval is short for a demo so a rule published mid-run converges within a turn
// or two. Production agents should leave the SDK default (5 minutes) — the conditional GET makes an
// unchanged poll nearly free, but not free enough to justify polling every few seconds.
const localControlPollInterval = 30 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "\nerror:", err)
		os.Exit(1)
	}
}

func run() error {
	userID := flag.String("user", "alice", "which seeded user to act on behalf of (alice|bob)")
	providerName := flag.String("provider", envOr("LLM_PROVIDER", ""), "llm provider: anthropic|openai|gemini (default: auto-detect from the key that is set)")
	model := flag.String("model", envOr("MODEL", ""), "model id (default: the provider's default)")
	input := flag.String("input", "", "run a single message and exit (no REPL)")
	serve := flag.Bool("serve", false, "run the OpenAI-compatible HTTP endpoint instead of the CLI")
	addr := flag.String("addr", envOr("AGENT_ADDR", ":8900"), "listen address for -serve")
	flag.Parse()

	u, ok := users[*userID]
	if !ok {
		return fmt.Errorf("unknown user %q (try: alice, bob)", *userID)
	}

	ctx := context.Background()

	// Optional: export guard decisions as OpenTelemetry spans to stdout (OTEL=1).
	telOpt, shutdownTel := setupTelemetry()
	defer shutdownTel()

	// 1. The guard: identity + gateway from env, a heartbeat so platform-driven mode flips
	//    (check ↔ passthrough ↔ gateway) reach the agent quickly, and a handler to surface them.
	//    The SDK's own default posture is warn (advisory warnings don't block, easing first
	//    integration); this example exists to show enforcement, so it defaults to enforce —
	//    FLYEDGE_MODE in the environment still wins.
	cfg := flyedge.LoadEnv()
	if os.Getenv("FLYEDGE_MODE") == "" {
		cfg.Mode = flyedge.ModeEnforce
	}
	guard, err := flyedge.New(cfg,
		flyedge.WithHeartbeat(15*time.Second),
		flyedge.WithModeChangeHandler(func(old, cur flyedge.ModelMode) {
			gov(cyan, "⚙  model mode changed: %s → %s", old, cur)
		}),
		telOpt,
	)
	if err != nil {
		return fmt.Errorf("build guard: %w", err)
	}
	defer guard.Close()

	// 2. Local controls: keep the org's in-process rule set current. Best-effort — an agent that
	//    cannot start the sync channel is still governed remotely, so log and continue.
	if err := guard.SyncLocalControls(
		flyedge.WithLocalControlInterval(localControlPollInterval),
		flyedge.WithLocalControlApplyHook(func(cfg localcontrol.Config, err error) {
			if err != nil {
				gov(yellow, "⚙  local controls: rejected published rules (%v) — keeping previous set", err)
				return
			}
			gov(cyan, "⚙  local controls: revision %d mode=%s detectors=%v",
				cfg.Version, cfg.Mode, guard.LocalControlDetectors())
		}),
	); err != nil {
		gov(yellow, "⚙  local controls: sync unavailable (%v) — remote enforcement unaffected", err)
	}

	// 3. ONE governed HTTP client shared by every model SDK: pre_llm on the way out, post_llm
	//    (WithResponseCheck) on the way back.
	hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport, flyedge.WithResponseCheck())}

	// Provider/model picker: prompt only for whatever wasn't explicitly flagged, and only when
	// stdin is interactive — a piped -input run or a container launch is never blocked on a menu.
	explicitProvider, explicitModel := false, false
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "provider":
			explicitProvider = true
		case "model":
			explicitModel = true
		}
	})
	if *input == "" && isInteractive() && (!explicitProvider || !explicitModel) {
		*providerName, *model = pickProviderAndModel(ctx, hc, *providerName, explicitProvider, explicitModel)
	}

	a, err := newAgent(guard, hc, *providerName, *model)
	if err != nil {
		return err
	}

	// Fail fast on a bad key or retired model — one cheap metadata call, before serving traffic.
	fmt.Fprintf(os.Stderr, "validating %s / %s ... ", a.provider.Name(), a.provider.Model())
	if err := a.provider.Validate(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED")
		return fmt.Errorf("%s key or model %q doesn't work: %w", a.provider.Name(), a.provider.Model(), err)
	}
	fmt.Fprintln(os.Stderr, "ok")

	// 4. Connect: publish the manifest (now that the real model + tools are known) and start
	//    presence/config polling. Best-effort — a local run without a reachable gateway still works
	//    as an ungoverned/observe agent.
	if err := guard.Connect(ctx, flyedge.ManifestInfo{
		Framework:   "reference-agent",
		Environment: envOr("COMPFLY_ENVIRONMENT", "prod"),
		Models:      []string{a.provider.Model()},
		Tools:       toolNames(),
	}); err != nil {
		gov(yellow, "⚙  Connect failed (%v) — running with fail-open checks", err)
	}

	fmt.Println()
	banner(guard, a.provider, u)
	fmt.Println()

	switch {
	case *serve:
		err = serveHTTP(ctx, a, u, *addr)
	case *input != "":
		err = runOnce(ctx, a, u, *input)
	default:
		err = chatLoop(ctx, a, u)
	}
	if err != nil {
		return err
	}

	fmt.Println("\n── protection report ───────────────────────────────")
	rep := guard.Report()
	fmt.Println(rep)
	if rep.Errors > 0 {
		fmt.Printf("\n%s⚠  %d of %d checks ERRORED — the gateway at %s was unreachable, so the guard\n"+
			"   failed OPEN and let those actions through UNENFORCED. This is not policy approval.\n"+
			"   Point COMPFLY_API_URL at a reachable gateway and re-run, or set FLYEDGE_FAIL_MODE=fail_closed\n"+
			"   to block instead of allowing when the gateway is down.%s\n",
			yellow, rep.Errors, rep.Checks, envOr("COMPFLY_API_URL", "(unset)"), reset)
	}
	return nil
}

// setupTelemetry returns a guard option + a shutdown func. With OTEL=1 it installs the OpenTelemetry
// sink behind a stdout exporter (spans print on shutdown); otherwise the guard uses its default
// in-memory recorder and the option is a no-op.
func setupTelemetry() (flyedge.Option, func()) {
	if os.Getenv("OTEL") == "" {
		return func(*flyedge.Guard) error { return nil }, func() {}
	}
	exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
	if err != nil {
		fmt.Fprintln(os.Stderr, "otel exporter:", err)
		return func(*flyedge.Guard) error { return nil }, func() {}
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp))
	otel.SetTracerProvider(tp)
	return flyedge.WithTelemetry(feotel.New(nil)), func() { _ = tp.Shutdown(context.Background()) }
}

// runOnce handles a single message and exits — scripted one-off governed tests.
func runOnce(ctx context.Context, a *agent, u *user, input string) error {
	fmt.Printf("%syou ▸ %s%s\n", bold, reset, input)
	reply, err := a.handle(ctx, u, "cli-"+randHex(), input)
	if err != nil {
		return err
	}
	printReply(reply)
	return nil
}

// chatLoop is the interactive REPL. Every turn shares one session id, so the platform sees a single
// continuous governed conversation (and per-session policy — rate/risk escalation — applies).
func chatLoop(ctx context.Context, a *agent, u *user) error {
	session := "cli-" + randHex()
	fmt.Printf("%stype a request, or /exit to quit%s\n\n", dim, reset)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for {
		fmt.Printf("%syou ▸ %s", bold, reset)
		if !sc.Scan() {
			fmt.Println()
			return nil
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if line == "/exit" || line == "/quit" {
			return nil
		}
		reply, err := a.handle(ctx, u, session, line)
		if err != nil {
			fmt.Printf("%serror: %v%s\n\n", red, err, reset)
			continue
		}
		printReply(reply)
		fmt.Println()
	}
}

// pickProviderAndModel prompts for whichever of provider/model wasn't explicitly flagged. The
// provider menu marks which key is present; the model menu is fetched LIVE from the chosen provider
// (a metadata call — no completion tokens — that doubles as an early signal the key works).
func pickProviderAndModel(ctx context.Context, hc *http.Client, providerName string, explicitProvider, explicitModel bool) (string, string) {
	opts := providerOptions()
	reader := bufio.NewReader(os.Stdin)

	chosen := providerName
	if !explicitProvider {
		def := providerName
		if def == "" {
			for _, o := range opts {
				if os.Getenv(o.EnvKey) != "" {
					def = o.Name
					break
				}
			}
		}
		if def == "" {
			def = opts[0].Name
		}
		fmt.Println("── choose an LLM provider ──────────────────────────")
		for i, o := range opts {
			keyMark := "✗ (no key)"
			if os.Getenv(o.EnvKey) != "" {
				keyMark = "✓ " + o.EnvKey
			}
			fmt.Printf("  %d) %-10s %-22s default model: %s\n", i+1, o.Name, keyMark, o.DefaultModel)
		}
		fmt.Printf("provider [%s]: ", def)
		pick := readLineOr(reader, def)
		if idx, err := strconv.Atoi(pick); err == nil && idx >= 1 && idx <= len(opts) {
			chosen = opts[idx-1].Name
		} else {
			chosen = pick
		}
	}

	if explicitModel {
		return chosen, ""
	}
	defaultModel := ""
	for _, o := range opts {
		if o.Name == chosen {
			defaultModel = o.DefaultModel
			break
		}
	}
	fmt.Printf("fetching %s models ... ", chosen)
	models, err := listModels(ctx, hc, chosen)
	if err != nil {
		fmt.Printf("unavailable (%v)\n", err)
		fmt.Printf("model [%s]: ", defaultModel)
		return chosen, readLineOr(reader, defaultModel)
	}
	fmt.Printf("%d found\n", len(models))
	for i, m := range models {
		fmt.Printf("  %d) %s\n", i+1, m)
	}
	fmt.Printf("model [%s]: ", defaultModel)
	pick := readLineOr(reader, defaultModel)
	if idx, err := strconv.Atoi(pick); err == nil && idx >= 1 && idx <= len(models) {
		pick = models[idx-1]
	}
	return chosen, pick
}

// isInteractive reports whether stdin looks like a terminal, so the picker never blocks piped or
// containerized runs. (A char-device check, not a tty ioctl — good enough for an example without
// pulling in x/term.)
func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func readLineOr(r *bufio.Reader, def string) string {
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func randHex() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
