// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	anthropicopt "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/openai/openai-go"
	openaiopt "github.com/openai/openai-go/option"
	"google.golang.org/genai"
)

const maxTurns = 8

// toolExec runs one tool call (gating + execution) and returns the governed content the provider
// should feed back to the model, whether it is an error result, and a step for the REPL's per-turn
// trace. Owned by the agent; called by the provider's loop.
type toolExec func(ctx context.Context, name string, args json.RawMessage) (result string, isErr bool, s step)

// provider is the pluggable LLM backend. It owns the SDK-specific tool-use loop; the guard governs
// every model call because each SDK client is built on the guard's governed http.Client.
type provider interface {
	Name() string
	Model() string
	Run(ctx context.Context, system, user string, tools []toolDef, exec toolExec) (text string, steps []step, err error)
	// ListModels returns the model ids this provider's key can currently use — for the startup
	// picker's model menu. A metadata-only call (no completion tokens spent).
	ListModels(ctx context.Context) ([]string, error)
	// Validate confirms the key + configured model actually work — one cheap metadata call, so a
	// bad key/retired model fails fast at startup instead of on the first real turn.
	Validate(ctx context.Context) error
}

// providerOption describes one selectable LLM backend — for the startup picker and the
// auto-detect/default logic, so both stay in sync off one list.
type providerOption struct {
	Name         string
	EnvKey       string
	DefaultModel string
}

func providerOptions() []providerOption {
	return []providerOption{
		{Name: "anthropic", EnvKey: "ANTHROPIC_API_KEY", DefaultModel: "claude-sonnet-4-5"},
		{Name: "openai", EnvKey: "OPENAI_API_KEY", DefaultModel: "gpt-4o"},
		{Name: "gemini", EnvKey: "GEMINI_API_KEY", DefaultModel: "gemini-3.5-flash"},
	}
}

// newProvider selects the backend. name "" auto-detects from which API key is present (first match
// in providerOptions order). model "" uses the provider's default. Every client is built on hc —
// the guard's governed http.Client — which is the whole point.
func newProvider(hc *http.Client, name, model string) (provider, error) {
	opts := providerOptions()
	if name == "" {
		for _, o := range opts {
			if os.Getenv(o.EnvKey) != "" {
				name = o.Name
				break
			}
		}
		if name == "" {
			return nil, fmt.Errorf("no LLM key found — set ANTHROPIC_API_KEY, OPENAI_API_KEY, or GEMINI_API_KEY (or pass -provider)")
		}
	}
	switch name {
	case "anthropic", "claude":
		if model == "" {
			model = "claude-sonnet-4-5"
		}
		return &anthropicProvider{
			client: anthropic.NewClient(anthropicopt.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")), anthropicopt.WithHTTPClient(hc)),
			model:  model,
		}, nil
	case "openai", "gpt":
		if model == "" {
			model = "gpt-4o"
		}
		return &openaiProvider{
			client: openai.NewClient(openaiopt.WithAPIKey(os.Getenv("OPENAI_API_KEY")), openaiopt.WithHTTPClient(hc)),
			model:  model,
		}, nil
	case "gemini":
		if model == "" {
			model = "gemini-3.5-flash"
		}
		client, err := genai.NewClient(context.Background(), &genai.ClientConfig{
			APIKey:     os.Getenv("GEMINI_API_KEY"),
			HTTPClient: hc,
			Backend:    genai.BackendGeminiAPI,
		})
		if err != nil {
			return nil, fmt.Errorf("gemini client: %w", err)
		}
		return &geminiProvider{client: client, model: model}, nil
	}
	return nil, fmt.Errorf("unknown provider %q (want anthropic|openai|gemini)", name)
}

// listModels lists the model ids available to providerName's key — for the startup picker, called
// before an agent (and its fixed model choice) exists.
func listModels(ctx context.Context, hc *http.Client, providerName string) ([]string, error) {
	p, err := newProvider(hc, providerName, "")
	if err != nil {
		return nil, err
	}
	return p.ListModels(ctx)
}

// --- Anthropic ---

type anthropicProvider struct {
	client anthropic.Client
	model  string
}

func (p *anthropicProvider) Name() string  { return "anthropic" }
func (p *anthropicProvider) Model() string { return p.model }

func (p *anthropicProvider) Run(ctx context.Context, system, user string, tools []toolDef, exec toolExec) (string, []step, error) {
	tps := make([]anthropic.ToolUnionParam, 0, len(tools))
	for _, t := range tools {
		tps = append(tps, anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
			Name:        t.name,
			Description: anthropic.String(t.description),
			InputSchema: anthropic.ToolInputSchemaParam{Properties: t.properties, Required: t.required},
		}})
	}
	msgs := []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(user))}
	var steps []step
	var text string
	for turn := 0; turn < maxTurns; turn++ {
		msg, err := p.client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     anthropic.Model(p.model),
			MaxTokens: 1024,
			System:    []anthropic.TextBlockParam{{Text: system}},
			Tools:     tps,
			Messages:  msgs,
		})
		if err != nil {
			return text, steps, err
		}
		msgs = append(msgs, msg.ToParam())
		var results []anthropic.ContentBlockParamUnion
		for _, b := range msg.Content {
			switch b.Type {
			case "text":
				if text != "" {
					text += "\n"
				}
				text += b.Text
			case "tool_use":
				tu := b.AsToolUse()
				content, isErr, s := exec(ctx, tu.Name, tu.Input)
				steps = append(steps, s)
				results = append(results, anthropic.NewToolResultBlock(tu.ID, content, isErr))
			}
		}
		if len(results) == 0 {
			break
		}
		msgs = append(msgs, anthropic.NewUserMessage(results...))
	}
	return text, steps, nil
}

func (p *anthropicProvider) ListModels(ctx context.Context) ([]string, error) {
	page, err := p.client.Models.List(ctx, anthropic.ModelListParams{})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(page.Data))
	for _, m := range page.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

func (p *anthropicProvider) Validate(ctx context.Context) error {
	_, err := p.client.Models.Get(ctx, p.model, anthropic.ModelGetParams{})
	return err
}

// --- OpenAI ---

type openaiProvider struct {
	client openai.Client
	model  string
}

func (p *openaiProvider) Name() string  { return "openai" }
func (p *openaiProvider) Model() string { return p.model }

func (p *openaiProvider) Run(ctx context.Context, system, user string, tools []toolDef, exec toolExec) (string, []step, error) {
	tps := make([]openai.ChatCompletionToolParam, 0, len(tools))
	for _, t := range tools {
		params := openai.FunctionParameters{"type": "object", "properties": t.properties}
		if len(t.required) > 0 {
			params["required"] = t.required
		}
		tps = append(tps, openai.ChatCompletionToolParam{Function: openai.FunctionDefinitionParam{
			Name:        t.name,
			Description: openai.String(t.description),
			Parameters:  params,
		}})
	}
	msgs := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(system), openai.UserMessage(user)}
	var steps []step
	var text string
	for turn := 0; turn < maxTurns; turn++ {
		resp, err := p.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:    openai.ChatModel(p.model),
			Messages: msgs,
			Tools:    tps,
		})
		if err != nil {
			return text, steps, err
		}
		if len(resp.Choices) == 0 {
			break
		}
		m := resp.Choices[0].Message
		if m.Content != "" {
			if text != "" {
				text += "\n"
			}
			text += m.Content
		}
		if len(m.ToolCalls) == 0 {
			break
		}
		msgs = append(msgs, m.ToParam())
		for _, tc := range m.ToolCalls {
			content, _, s := exec(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			steps = append(steps, s)
			msgs = append(msgs, openai.ToolMessage(content, tc.ID))
		}
	}
	return text, steps, nil
}

// openaiNonChatModel matches model ids OpenAI's /v1/models returns that aren't chat/completions
// models — excluded from the picker.
var openaiNonChatModel = []string{
	"embedding", "whisper", "tts", "transcribe", "realtime", "audio",
	"dall-e", "davinci", "babbage", "moderation", "image",
}

func (p *openaiProvider) ListModels(ctx context.Context) ([]string, error) {
	page, err := p.client.Models.List(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(page.Data))
	for _, m := range page.Data {
		id := strings.ToLower(m.ID)
		skip := false
		for _, bad := range openaiNonChatModel {
			if strings.Contains(id, bad) {
				skip = true
				break
			}
		}
		if !skip {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (p *openaiProvider) Validate(ctx context.Context) error {
	_, err := p.client.Models.Get(ctx, p.model)
	return err
}

// --- Gemini ---

type geminiProvider struct {
	client *genai.Client
	model  string
}

func (p *geminiProvider) Name() string  { return "gemini" }
func (p *geminiProvider) Model() string { return p.model }

func (p *geminiProvider) Run(ctx context.Context, system, user string, tools []toolDef, exec toolExec) (string, []step, error) {
	var gTools []*genai.Tool
	if len(tools) > 0 {
		decls := make([]*genai.FunctionDeclaration, 0, len(tools))
		for _, t := range tools {
			schema := map[string]any{"type": "object", "properties": t.properties}
			if len(t.required) > 0 {
				schema["required"] = t.required
			}
			decls = append(decls, &genai.FunctionDeclaration{
				Name:                 t.name,
				Description:          t.description,
				ParametersJsonSchema: schema,
			})
		}
		gTools = []*genai.Tool{{FunctionDeclarations: decls}}
	}

	cfg := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{Parts: []*genai.Part{genai.NewPartFromText(system)}},
		Tools:             gTools,
	}
	contents := []*genai.Content{genai.NewContentFromText(user, genai.RoleUser)}

	var steps []step
	var text string
	for turn := 0; turn < maxTurns; turn++ {
		resp, err := p.client.Models.GenerateContent(ctx, p.model, contents, cfg)
		if err != nil {
			return text, steps, err
		}
		if len(resp.Candidates) == 0 {
			break
		}
		if t := resp.Text(); t != "" {
			if text != "" {
				text += "\n"
			}
			text += t
		}
		calls := resp.FunctionCalls()
		if len(calls) == 0 {
			break
		}
		contents = append(contents, resp.Candidates[0].Content)
		respParts := make([]*genai.Part, 0, len(calls))
		for _, fc := range calls {
			args, _ := json.Marshal(fc.Args)
			content, _, s := exec(ctx, fc.Name, json.RawMessage(args))
			steps = append(steps, s)
			respParts = append(respParts, genai.NewPartFromFunctionResponse(fc.Name, geminiFunctionResponse(content)))
		}
		contents = append(contents, &genai.Content{Role: genai.RoleUser, Parts: respParts})
	}
	return text, steps, nil
}

// geminiFunctionResponse adapts a tool's result into the map Gemini's FunctionResponse.Response
// requires: pass a JSON object through as-is, wrap anything else under "output" per the field's
// documented convention.
func geminiFunctionResponse(content string) map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(content), &m) == nil {
		return m
	}
	return map[string]any{"output": content}
}

// geminiNonChatModel excludes model families the picker shouldn't offer for a text/tool-use chat
// loop (TTS, image/video generation, embeddings, live/realtime, Q&A-only) — everything else under
// "gemini-" is a generateContent-capable chat model.
var geminiNonChatModel = []string{"tts", "embedding", "aqa", "imagen", "veo", "live"}

func (p *geminiProvider) ListModels(ctx context.Context) ([]string, error) {
	var ids []string
	for m, err := range p.client.Models.All(ctx) {
		if err != nil {
			return ids, err
		}
		id := strings.ToLower(strings.TrimPrefix(m.Name, "models/"))
		if !strings.HasPrefix(id, "gemini-") {
			continue
		}
		skip := false
		for _, bad := range geminiNonChatModel {
			if strings.Contains(id, bad) {
				skip = true
				break
			}
		}
		if !skip {
			ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (p *geminiProvider) Validate(ctx context.Context) error {
	_, err := p.client.Models.Get(ctx, p.model, nil)
	return err
}
