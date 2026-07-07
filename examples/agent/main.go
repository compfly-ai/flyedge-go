// Command agent is a minimal flyedge-protected agent. It builds a flyedge.Guard from the agent's
// DID identity + the prism gateway, then installs ONE governed http.Client (guard.WrapRoundTripper)
// into both the Anthropic and OpenAI SDKs — proving a single, provider-agnostic transport wrap
// governs any HTTP LLM client, no per-framework adapter required. The pre_llm policy check runs
// against prism before the model call; a policy Deny returns a *flyedge.DenyError and the provider
// is never called.
//
// Env:
//   COMPFLY_API_URL               prism base (e.g. http://localhost:8080)
//   COMPFLY_AGENT_DID             the agent's DID (MCP-minted)
//   COMPFLY_AGENT_PRIVATE_KEY_PATH  Ed25519 PEM
//   FLYEDGE_MODE                  enforce|warn (default warn)
//   PROMPT                        the user prompt (default: a benign question)
//   ANTHROPIC_API_KEY             use Claude (default provider)
//   OPENAI_API_KEY                set PROVIDER=openai to use GPT instead
//   PROVIDER                      anthropic (default) | openai
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	flyedge "github.com/compfly-ai/flyedge-go"
	"github.com/openai/openai-go"
	openaiopt "github.com/openai/openai-go/option"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	// 1. Build the guard from the agent's identity + gateway (explicit; no globals).
	guard, err := flyedge.New(flyedge.LoadEnv())
	if err != nil {
		return fmt.Errorf("flyedge: %w", err)
	}
	defer guard.Close()
	fmt.Printf("flyedge guard: DID=%s mode=%s\n", guard.DID(), envOr("FLYEDGE_MODE", "warn"))

	// 2. ONE governed HTTP client — the same wrap works for every HTTP LLM SDK.
	hc := &http.Client{Transport: guard.WrapRoundTripper(http.DefaultTransport, flyedge.WithResponseCheck())}

	prompt := envOr("PROMPT", "What are your store hours?")
	fmt.Printf("provider=%s prompt=%q\n", envOr("PROVIDER", "anthropic"), prompt)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var reply string
	switch envOr("PROVIDER", "anthropic") {
	case "openai":
		reply, err = askOpenAI(ctx, hc, prompt)
	default:
		reply, err = askClaude(ctx, hc, prompt)
	}
	if err != nil {
		if de, ok := flyedge.AsDenyError(err); ok {
			fmt.Printf("BLOCKED by policy: %s\n", de.Decision.Reason)
		} else {
			return err
		}
	} else {
		fmt.Printf("reply: %s\n", reply)
	}

	// Protection summary — printed explicitly by the caller (not auto-emitted).
	fmt.Println(guard.Report())
	return nil
}

// askClaude sends the prompt to Claude through the governed http client (Anthropic SDK).
func askClaude(ctx context.Context, hc *http.Client, prompt string) (string, error) {
	client := anthropic.NewClient(
		anthropicopt.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
		anthropicopt.WithHTTPClient(hc),
	)
	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 256,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(prompt))},
	})
	if err != nil {
		return "", err
	}
	var out string
	for _, b := range msg.Content {
		if b.Type == "text" {
			out += b.Text
		}
	}
	return out, nil
}

// askOpenAI sends the prompt to GPT through the SAME governed http client (OpenAI SDK).
func askOpenAI(ctx context.Context, hc *http.Client, prompt string) (string, error) {
	client := openai.NewClient(
		openaiopt.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
		openaiopt.WithHTTPClient(hc),
	)
	resp, err := client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    openai.ChatModelGPT4o,
		Messages: []openai.ChatCompletionMessageParamUnion{openai.UserMessage(prompt)},
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", nil
	}
	return resp.Choices[0].Message.Content, nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
