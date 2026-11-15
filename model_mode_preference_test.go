// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 CompFly AI

package flyedge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type modeRecordingTransport struct{ url, preference string }

func (r *modeRecordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.url = req.URL.String()
	r.preference = req.Header.Get("X-CompFly-Model-Mode")
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"choices":[{"message":{"content":"ok"}}]}`))}, nil
}
func applyModeJSON(t *testing.T, g *Guard, body string) {
	t.Helper()
	var c flyedgeConfigResponse
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	g.applyConfig(c)
}
func TestInheritedGatewayRoutesBothModelProtocols(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			e := &responsesEnforcer{}
			g := responsesGuard(t, Config{APIURL: "https://gateway.example"}, e)
			applyModeJSON(t, g, `{"model_mode":"gateway","model_mode_source":"org_default"}`)
			base := &modeRecordingTransport{}
			req, _ := http.NewRequest("POST", "https://api.openai.com"+path, strings.NewReader(`{"model":"test","messages":[],"input":"hi"}`))
			req.Header.Set("X-CompFly-Model-Mode", "passthrough")
			resp, err := g.WrapRoundTripper(base).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if base.url != "https://gateway.example/v1/proxy" || base.preference != "" || len(e.requests) != 0 {
				t.Fatalf("url=%s preference=%s checks=%d", base.url, base.preference, len(e.requests))
			}
		})
	}
}

func TestModelModePreferenceRouting(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sdk            ModelMode
		legacy         bool
		server, source string
		effective      ModelMode
		header         string
	}{
		{name: "inherit absent", effective: ModelModeCheck},
		{name: "SDK check beats org gateway", sdk: ModelModeCheck, server: "gateway", source: "org_default", effective: ModelModeCheck},
		{name: "SDK gateway beats org check", sdk: ModelModeGateway, server: "check", source: "org_default", effective: ModelModeGateway, header: "gateway"},
		{name: "SDK passthrough beats fallback", sdk: ModelModePassthrough, server: "check", source: "fallback", effective: ModelModePassthrough, header: "passthrough"},
		{name: "agent check beats SDK gateway", sdk: ModelModeGateway, server: "check", source: "per_agent_did_override", effective: ModelModeCheck},
		{name: "agent gateway beats SDK check", sdk: ModelModeCheck, server: "gateway", source: "per_agent_did_override", effective: ModelModeGateway, header: "check"},
		{name: "old server check remains authoritative", sdk: ModelModeGateway, server: "check", effective: ModelModeCheck},
		{name: "unknown source remains authoritative", sdk: ModelModeCheck, server: "passthrough", source: "future_source", effective: ModelModePassthrough, header: "check"},
		{name: "inherit gateway", server: "gateway", source: "org_default", effective: ModelModeGateway},
		{name: "legacy absent", legacy: true, effective: ModelModePassthrough, header: "passthrough"},
		{name: "legacy org gateway", legacy: true, server: "gateway", source: "org_default", effective: ModelModeGateway, header: "gateway"},
		{name: "legacy org check", legacy: true, server: "check", source: "org_default", effective: ModelModePassthrough, header: "passthrough"},
		{name: "legacy agent check", legacy: true, server: "check", source: "per_agent_did_override", effective: ModelModeCheck},
		{name: "explicit check beats legacy", sdk: ModelModeCheck, legacy: true, effective: ModelModeCheck},
	} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
			t.Run(tc.name+path, func(t *testing.T) {
				e := &responsesEnforcer{}
				g := responsesGuard(t, Config{APIURL: "https://gateway.example", ModelMode: tc.sdk, ProxyMode: tc.legacy}, e)
				if tc.server != "" {
					raw, _ := json.Marshal(map[string]string{"model_mode": tc.server, "model_mode_source": tc.source})
					applyModeJSON(t, g, string(raw))
				}
				if g.ModelMode() != tc.effective {
					t.Fatalf("mode=%s want=%s", g.ModelMode(), tc.effective)
				}
				base := &modeRecordingTransport{}
				req, _ := http.NewRequest("POST", "https://api.openai.com"+path, strings.NewReader(`{"model":"test","messages":[],"input":"hi"}`))
				resp, err := g.WrapRoundTripper(base).RoundTrip(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				wantURL := "https://gateway.example/v1/proxy"
				checks := 0
				if tc.effective == ModelModeCheck {
					wantURL = "https://api.openai.com" + path
					checks = 2
				}
				if base.url != wantURL || base.preference != tc.header || len(e.requests) != checks {
					t.Fatalf("route=%s header=%s checks=%d", base.url, base.preference, len(e.requests))
				}
			})
		}
	}
}

type modeWireEnforcer struct {
	responsesEnforcer
	connectBody []byte
}

func (e *modeWireEnforcer) PostSigned(_ context.Context, _ string, _ []byte) ([]byte, error) {
	return e.connectBody, nil
}
func TestModelPreferenceSurvivesConnectHeartbeatAndReconnect(t *testing.T) {
	e := &modeWireEnforcer{connectBody: []byte(`{"model_mode":"gateway","model_mode_source":"org_default"}`)}
	var changes [][2]ModelMode
	g, err := New(Config{ModelMode: ModelModeCheck}, WithoutCloudTelemetry(), WithEnforcer(e), WithModeChangeHandler(func(old, current ModelMode) { changes = append(changes, [2]ModelMode{old, current}) }))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err := g.connectOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	applyModeJSON(t, g, `{"model_mode":"gateway","model_mode_source":"org_default"}`)
	if g.ModelMode() != ModelModeCheck || len(changes) != 0 {
		t.Fatal("org default replaced SDK preference")
	}
	applyModeJSON(t, g, `{"model_mode":"gateway","model_mode_source":"per_agent_did_override"}`)
	applyModeJSON(t, g, `{"model_mode":"gateway","model_mode_source":"per_agent_did_override"}`)
	applyModeJSON(t, g, `{"model_mode":"gateway","model_mode_source":"org_default"}`)
	applyModeJSON(t, g, `{"model_mode":"passthrough"}`)
	applyModeJSON(t, g, `{}`)
	applyModeJSON(t, g, `{"model_mode":"invalid","model_mode_source":"fallback"}`)
	if g.ModelMode() != ModelModePassthrough {
		t.Fatal("missing/invalid response erased known server mode")
	}
	if err := g.reconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][2]ModelMode{{ModelModeCheck, ModelModeGateway}, {ModelModeGateway, ModelModeCheck}, {ModelModeCheck, ModelModePassthrough}, {ModelModePassthrough, ModelModeCheck}}
	if !reflect.DeepEqual(changes, want) || g.ModelMode() != ModelModeCheck {
		t.Fatalf("changes=%v mode=%s", changes, g.ModelMode())
	}
}
func TestModelModeConfigValidationAndEnv(t *testing.T) {
	for _, mode := range []string{"check", "passthrough", "gateway", ""} {
		t.Setenv("FLYEDGE_MODEL_MODE", mode)
		cfg := LoadEnv()
		if cfg.ModelMode != ModelMode(mode) {
			t.Fatal("environment preference missing")
		}
		g, err := New(cfg, WithoutCloudTelemetry())
		if err != nil {
			t.Fatal(err)
		}
		g.Close()
	}
	for _, mode := range []string{"invalid", "CHECK", " "} {
		t.Setenv("FLYEDGE_MODEL_MODE", mode)
		if g, err := New(LoadEnv(), WithoutCloudTelemetry()); err == nil {
			g.Close()
			t.Fatalf("invalid mode %q accepted", mode)
		}
	}
}
func TestExplicitCheckStillEnforcesDeny(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			e := &responsesEnforcer{deny: StagePreLLM}
			g := responsesGuard(t, Config{ModelMode: ModelModeCheck}, e)
			applyModeJSON(t, g, `{"model_mode":"gateway","model_mode_source":"org_default"}`)
			base := &modeRecordingTransport{}
			req, _ := http.NewRequest("POST", "https://api.openai.com"+path, strings.NewReader(`{"model":"test","messages":[],"input":"hi"}`))
			if _, err := g.WrapRoundTripper(base).RoundTrip(req); err == nil || base.url != "" {
				t.Fatal("deny reached provider")
			}
		})
	}
}

func TestDirectModelCallsStripReservedModeHeader(t *testing.T) {
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			e := &responsesEnforcer{}
			g := responsesGuard(t, Config{ModelMode: ModelModeCheck}, e)
			base := &modeRecordingTransport{}
			req, _ := http.NewRequest("POST", "https://api.openai.com"+path, strings.NewReader(`{"model":"test","messages":[],"input":"hi"}`))
			req.Header.Set("X-CompFly-Model-Mode", "gateway")
			response, err := g.WrapRoundTripper(base).RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if base.url != "https://api.openai.com"+path || base.preference != "" {
				t.Fatalf("provider route=%s leaked preference=%q", base.url, base.preference)
			}
		})
	}
}
