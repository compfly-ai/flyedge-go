package flyedge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/compfly-ai/flyedge-go/simulation"
)

// sdkVersion identifies this SDK in the manifest + telemetry.
const sdkVersion = "flyedge-go/0.1.0"

// signedPoster is implemented by the default HTTP enforcer; it lets Connect + cloud telemetry reuse
// the enforcer's signed POST without widening the Enforcer interface (stub enforcers in tests need
// not implement it — Connect then reports it's unavailable).
type signedPoster interface {
	PostSigned(ctx context.Context, path string, body []byte) ([]byte, error)
}

// ManifestInfo is what an agent declares about itself at Connect: its framework and the tools/
// models it uses. The platform uses this for presence + manifest-seeded behavioral baselines.
type ManifestInfo struct {
	Framework   string   // e.g. "langchaingo", "anthropic-sdk-go"
	Tools       []string // tool names the agent can call
	Models      []string // model ids the agent uses
	Environment string   // dev|staging|prod
}

// Connect registers the agent's manifest with the gateway (POST /v1/flyedge/connect), enabling
// presence tracking and manifest-seeded baselines. Explicit — call it once at startup. Requires a
// signed enforcer (a real Guard); a stub/offline enforcer returns an error.
//
// On success it also starts the config heartbeat poller (an owned goroutine, stopped by Close):
// the ConnectResponse carries the heartbeat cadence + initial model_mode, and the poller then keeps
// model_mode current, honors manifest-refresh requests, and surfaces the simulation block.
func (g *Guard) Connect(ctx context.Context, info ManifestInfo) error {
	g.pollMu.Lock()
	g.manifestInfo = info
	// Build the simulation controller (unless opted out) + wire the poller's sim hook to it, so an
	// active run makes this agent an observe-mode target with no extra caller code.
	if g.simEnabled && g.simCtl == nil {
		fw := info.Framework
		if fw == "" {
			fw = sdkVersion
		}
		g.simCtl = simulation.New(fw)
		g.onSimChange = func(sc *SimulationConfig) { g.simCtl.OnConfigChange(g.toSimConfig(sc)) }
	}
	g.pollMu.Unlock()
	if err := g.connectOnce(ctx); err != nil {
		return err
	}
	g.startConfigPoll()
	return nil
}

// connectOnce POSTs the current manifest and folds the ConnectResponse (cadence + model_mode) into
// the Guard. Used by both Connect and the manifest-refresh path (reconnect).
func (g *Guard) connectOnce(ctx context.Context) error {
	sp, ok := g.enforcer.(signedPoster)
	if !ok {
		return fmt.Errorf("flyedge: Connect requires the default HTTP enforcer")
	}
	g.pollMu.RLock()
	info := g.manifestInfo
	g.pollMu.RUnlock()

	m := buildManifest(info)
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	raw, err := sp.PostSigned(ctx, "/v1/flyedge/connect", body)
	if err != nil {
		return fmt.Errorf("flyedge: connect: %w", err)
	}

	var cr connectResponse
	_ = json.Unmarshal(raw, &cr) // response parse is best-effort — a 200 already means accepted.
	g.pollMu.Lock()
	g.manifestHash = m.ManifestHash
	if cr.ModelMode != nil {
		g.modelMode = ModelMode(*cr.ModelMode)
	}
	if g.pollInterval <= 0 && cr.HeartbeatIntervalSeconds > 0 {
		g.pollInterval = time.Duration(cr.HeartbeatIntervalSeconds) * time.Second
	}
	g.pollMu.Unlock()
	return nil
}

// reconnect re-sends the manifest when prism requests a refresh (manifest_refresh_required). It does
// not restart the poller — that runs for the Guard's lifetime.
func (g *Guard) reconnect(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()
	return g.connectOnce(ctx)
}

// --- manifest wire shape (matches prism AgentManifest) ---

type agentManifest struct {
	SDKVersion   string            `json:"sdk_version"`
	ManifestHash string            `json:"manifest_hash"`
	Framework    frameworkInfo     `json:"framework"`
	Capabilities agentCapabilities `json:"capabilities"`
	Environment  *environmentInfo  `json:"environment,omitempty"`
}

type frameworkInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Capability arrays are omitted when empty: the gateway treats an absent key as "preserve
// existing" rather than "agent reports zero".
type agentCapabilities struct {
	Tools  []toolCapability  `json:"tools,omitempty"`
	Models []modelCapability `json:"models,omitempty"`
}

type toolCapability struct {
	Name string `json:"name"`
}

type modelCapability struct {
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`
}

type environmentInfo struct {
	Name string `json:"name"`
}

func buildManifest(info ManifestInfo) agentManifest {
	fw := info.Framework
	if fw == "" {
		fw = "flyedge-go"
	}
	var caps agentCapabilities
	for _, t := range info.Tools {
		caps.Tools = append(caps.Tools, toolCapability{Name: t})
	}
	for _, m := range info.Models {
		caps.Models = append(caps.Models, modelCapability{Provider: providerFor(m), ModelID: m})
	}
	m := agentManifest{
		SDKVersion:   sdkVersion,
		Framework:    frameworkInfo{Name: fw, Version: sdkVersion},
		Capabilities: caps,
	}
	if info.Environment != "" {
		m.Environment = &environmentInfo{Name: info.Environment}
	}
	m.ManifestHash = manifestHash(m)
	return m
}

// providerFor infers a model's provider from its id (best-effort; the manifest wants a provider).
func providerFor(model string) string {
	switch {
	case strings.HasPrefix(model, "claude"):
		return "anthropic"
	case strings.HasPrefix(model, "gpt") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3"):
		return "openai"
	default:
		return "unknown"
	}
}

// manifestHash is a stable content hash over the declared capabilities + framework (prism uses it
// to detect manifest drift and request a refresh).
func manifestHash(m agentManifest) string {
	h := struct {
		F frameworkInfo     `json:"f"`
		C agentCapabilities `json:"c"`
	}{m.Framework, m.Capabilities}
	b, _ := json.Marshal(h)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
