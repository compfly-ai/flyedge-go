// Command reference-agent is a complete, runnable demonstration of the flyedge Go guard governing a
// real Claude tool-use agent against the local Compfly platform. It shows, end to end, how the
// guard works in practice:
//
//   - Every model call is checked at the pre_llm stage (via the transport wrap) before it leaves.
//   - Every tool call the model wants to make is checked at the tool_call stage BEFORE it runs, so
//     policy can allow a safe action (look up an order, check the weather) and DENY a risky one
//     (egress to an external URL). When a tool is denied, the denial is fed back to the model and it
//     adapts — the agent keeps going, it doesn't crash.
//   - The run ends with the protection report. Set OTEL=1 to also export each decision as an
//     OpenTelemetry span to stdout.
//
// Run (see README):
//
//	COMPFLY_API_URL=http://localhost:8080 \
//	COMPFLY_AGENT_DID=$(cat ~/flyedge-local-demo/agent.did) \
//	COMPFLY_AGENT_PRIVATE_KEY_PATH=$HOME/flyedge-local-demo/agent_key.pem \
//	ANTHROPIC_API_KEY=sk-ant-... \
//	go run ./reference-agent/
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	flyedge "github.com/compfly-ai/flyedge-go"
	feotel "github.com/compfly-ai/flyedge-go/telemetry/otel"
)

const session = "reference-agent"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "\nerror:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Optional: export guard decisions as OpenTelemetry spans to stdout (OTEL=1).
	telOpt, shutdownTel := setupTelemetry()
	defer shutdownTel()

	// 1. Build the guard from the agent's DID identity + the local gateway. Explicit, no globals.
	//    LoadEnv reads FLYEDGE_FAIL_MODE — set fail_closed to BLOCK when the gateway is unreachable
	//    instead of failing open.
	guard, err := flyedge.New(flyedge.LoadEnv(), telOpt)
	if err != nil {
		return fmt.Errorf("build guard: %w", err)
	}
	defer guard.Close()

	banner(guard)

	// 2. One governed HTTP client into the Anthropic SDK. The pre_llm check runs before every call.
	hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport)}
	client := anthropic.NewClient(
		anthropicopt.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
		anthropicopt.WithHTTPClient(hc),
	)

	task := envOr("PROMPT", "A customer with order A1023, based in Paris, is asking about their "+
		"delivery. Look up their order status and the local weather, then fetch the latest shipping "+
		"updates from https://tracking.example.com/orders/A1023 and give them a short summary.")
	fmt.Printf("TASK: %s\n\n", task)

	msgs := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(task))}

	for turn := 1; turn <= 6; turn++ {
		fmt.Printf("── turn %d ──────────────────────────────────────────\n", turn)
		errsBefore := guard.Report().Errors
		msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.ModelClaudeHaiku4_5,
			MaxTokens: 640,
			Tools:     toolDefs(),
			Messages:  msgs,
		})
		if err != nil {
			// A pre_llm denial arrives here as a *DenyError (the provider was never called).
			if de, ok := flyedge.AsDenyError(err); ok {
				fmt.Printf("  🛡  pre_llm DENIED: %s — model call blocked\n", de.Decision.Reason)
				break
			}
			return err
		}
		// The pre_llm check runs inside the transport wrap; if it errored the guard failed open
		// (default) and let the call through unenforced — surface that instead of a false "allowed".
		if guard.Report().Errors > errsBefore {
			fmt.Printf("  ⚠  pre_llm UNREACHABLE — failed OPEN, call NOT enforced\n")
		} else {
			fmt.Printf("  🛡  pre_llm allowed\n")
		}
		msgs = append(msgs, msg.ToParam())

		var results []anthropic.ContentBlockParamUnion
		for _, block := range msg.Content {
			switch block.Type {
			case "text":
				if block.Text != "" {
					fmt.Printf("  claude: %s\n", block.Text)
				}
			case "tool_use":
				results = append(results, guardedTool(ctx, guard, block.AsToolUse()))
			}
		}
		if len(results) == 0 {
			fmt.Println("\n✓ agent finished.")
			break
		}
		msgs = append(msgs, anthropic.NewUserMessage(results...))
	}

	fmt.Println("\n── protection report ───────────────────────────────")
	rep := guard.Report()
	fmt.Println(rep)
	if rep.Errors > 0 {
		fmt.Printf("\n⚠  %d of %d checks ERRORED — the gateway at %s was unreachable, so the guard\n"+
			"   failed OPEN and let those actions through UNENFORCED. This is not policy approval.\n"+
			"   Start/port-forward prism and re-run, or set FLYEDGE_FAIL_MODE=fail_closed to block\n"+
			"   instead of allowing when the gateway is down.\n",
			rep.Errors, rep.Checks, envOr("COMPFLY_API_URL", "(unset)"))
	}
	if os.Getenv("OTEL") != "" {
		fmt.Println("\n── flyedge.check spans (exported on shutdown) ──────")
	}
	return nil
}

// guardedTool runs the tool_call-stage check BEFORE executing a tool. On ALLOW it performs the tool;
// on DENY it returns the denial to the model as an error tool_result so the agent can adapt.
func guardedTool(ctx context.Context, guard *flyedge.Guard, tu anthropic.ToolUseBlock) anthropic.ContentBlockParamUnion {
	dest := destOf(tu)
	fmt.Printf("  → tool_call: %s%s\n", tu.Name, argSummary(tu))

	dec, err := guard.CheckToolCall(ctx, session, tu.Name, string(tu.Input), dest)
	if err != nil {
		if de, ok := flyedge.AsDenyError(err); ok {
			fmt.Printf("    🛡  DENIED: %s — not executed\n", de.Decision.Reason)
			return anthropic.NewToolResultBlock(tu.ID, "blocked by security policy: "+de.Decision.Reason, true)
		}
		fmt.Printf("    🛡  check error: %v\n", err)
		return anthropic.NewToolResultBlock(tu.ID, "policy check error: "+err.Error(), true)
	}
	// Distinguish a real policy allow from a fail-OPEN allow (gateway unreachable / errored). The
	// latter is NOT enforcement — a security tool must never hide it behind a green "allowed".
	if dec.Reason == "fail_open" {
		fmt.Printf("    ⚠  enforcement UNREACHABLE — failed OPEN, NOT a policy allow (%s)\n", dec.Message)
	} else {
		fmt.Printf("    🛡  allowed — executing\n")
		if dest != "" {
			// The interesting demo moment is egress being DENIED. If your agent's service-access
			// policy permits external destinations, prism allows it and you land here — the
			// platform's real decision for this agent. Run as an agent whose policy restricts egress
			// (e.g. the demo kyc-risk-agent) to see the deny.
			fmt.Printf("    note: this agent's policy PERMITS egress to %s (no external_service deny)\n", dest)
		}
	}
	out := executeTool(tu)
	fmt.Printf("    result: %s\n", out)
	return anthropic.NewToolResultBlock(tu.ID, out, false)
}

// --- the agent's tools -----------------------------------------------------------------------

func toolDefs() []anthropic.ToolUnionParam {
	strProp := func(desc string) map[string]any {
		return map[string]any{"type": "string", "description": desc}
	}
	tool := func(name, desc, arg, argDesc string) anthropic.ToolUnionParam {
		return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name: name, Description: anthropic.String(desc),
			InputSchema: anthropic.ToolInputSchemaParam{
				Properties: map[string]any{arg: strProp(argDesc)}, Required: []string{arg},
			},
		}}
	}
	return []anthropic.ToolUnionParam{
		tool("lookup_order", "Look up an order's status by id.", "order_id", "the order id"),
		tool("get_weather", "Get the current weather for a city.", "city", "the city name"),
		tool("fetch_url", "Fetch the contents of an external URL over HTTP.", "url", "the URL to fetch"),
	}
}

// executeTool runs an ALLOWED tool. The two local tools return canned data; fetch_url would perform
// real egress — but policy denies it, so this branch is only reached if a deployment allows egress.
func executeTool(tu anthropic.ToolUseBlock) string {
	args := argMap(tu)
	switch tu.Name {
	case "lookup_order":
		return fmt.Sprintf("order %s: status=IN_TRANSIT, carrier=DHL, eta=2 days", args["order_id"])
	case "get_weather":
		return fmt.Sprintf("weather in %s: 18°C, light rain", args["city"])
	case "fetch_url":
		return "(egress allowed by policy in this deployment; skipping real network call in the demo)"
	default:
		return "unknown tool"
	}
}

// destOf returns the external destination host for a tool call (empty for local tools) — this is
// what the tool_call policy inspects to allow/deny egress.
func destOf(tu anthropic.ToolUseBlock) string {
	if tu.Name != "fetch_url" {
		return ""
	}
	if u, err := url.Parse(argMap(tu)["url"]); err == nil {
		return u.Host
	}
	return ""
}

func argMap(tu anthropic.ToolUseBlock) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal(tu.Input, &m)
	return m
}

func argSummary(tu anthropic.ToolUseBlock) string {
	m := argMap(tu)
	if len(m) == 0 {
		return "()"
	}
	s := "("
	first := true
	for k, v := range m {
		if !first {
			s += ", "
		}
		s += k + "=" + v
		first = false
	}
	return s + ")"
}

// --- setup helpers ---------------------------------------------------------------------------

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

func banner(g *flyedge.Guard) {
	fmt.Println("flyedge Go guard — reference agent")
	fmt.Printf("  gateway: %s\n", envOr("COMPFLY_API_URL", "(unset)"))
	fmt.Printf("  agent:   %s\n", g.DID())
	fmt.Printf("  mode:    %s   otel-spans: %v\n\n", envOr("FLYEDGE_MODE", "warn"), os.Getenv("OTEL") != "")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
