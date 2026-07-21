package flyedge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// configPath is the gateway endpoint the poller GETs. Its response carries the resolved model_mode,
// a manifest-refresh flag, and (when a run is active) the simulation config — the channel through
// which the platform pushes dynamic state to a live agent.
const configPath = "/v1/flyedge/config"

// ModelMode is prism's resolved routing mode for an agent. The poller keeps it current; the
// transport wrap (later phases) consults it to decide how to route model calls.
type ModelMode string

const (
	ModelModeCheck       ModelMode = "check"       // default: call the provider directly, check via /v1/flyedge/check
	ModelModePassthrough ModelMode = "passthrough" // route through prism carrying the agent's own key
	ModelModeGateway     ModelMode = "gateway"     // send to prism; prism supplies agent-scoped credentials
)

// SimulationConfig is prism's `simulation` block from GET /v1/flyedge/config (frozen wire —
// matches prism SimulationConfig). Delivered only while a run is active. Phase B's controller reacts
// to it; Phase A surfaces it via Guard.SimulationConfig / SimulationActive.
type SimulationConfig struct {
	Active             bool            `json:"active"`
	RunID              string          `json:"run_id"`
	Middlewares        []string        `json:"middlewares"`
	TelemetryJWT       string          `json:"telemetry_jwt"`
	TelemetryURL       string          `json:"telemetry_url"`
	ProtectionDisabled bool            `json:"protection_disabled"`
	Extra              json.RawMessage `json:"extra,omitempty"`
}

// flyedgeConfigResponse is the GET /v1/flyedge/config body. All fields are optional (prism omits
// them when null).
type flyedgeConfigResponse struct {
	Simulation               *SimulationConfig `json:"simulation,omitempty"`
	ManifestRefreshRequired  *bool             `json:"manifest_refresh_required,omitempty"`
	HeartbeatIntervalSeconds *uint32           `json:"heartbeat_interval_seconds,omitempty"`
	ModelMode                *string           `json:"model_mode,omitempty"`
}

// connectResponse is the POST /v1/flyedge/connect body: the heartbeat cadence + initial model mode.
type connectResponse struct {
	Accepted                 bool    `json:"accepted"`
	HeartbeatIntervalSeconds uint32  `json:"heartbeat_interval_seconds"`
	ModelMode                *string `json:"model_mode,omitempty"`
}

// signedGetter is the optional enforcer capability the poller needs (a signed GET). The default
// HTTPEnforcer implements it; stub enforcers in tests need not — the poller simply doesn't start.
type signedGetter interface {
	GetSigned(ctx context.Context, path string, headers map[string]string) ([]byte, error)
}

// ModelMode returns the agent's current routing mode (default ModelModeCheck until the first poll).
func (g *Guard) ModelMode() ModelMode {
	g.pollMu.RLock()
	defer g.pollMu.RUnlock()
	if g.modelMode == "" {
		return ModelModeCheck
	}
	return g.modelMode
}

// SimulationActive reports whether the last config poll saw an active simulation run.
func (g *Guard) SimulationActive() bool {
	g.pollMu.RLock()
	defer g.pollMu.RUnlock()
	return g.sim != nil && g.sim.Active
}

// SimulationConfig returns a copy of the last-seen simulation block, or nil if none is active.
func (g *Guard) SimulationConfig() *SimulationConfig {
	g.pollMu.RLock()
	defer g.pollMu.RUnlock()
	if g.sim == nil {
		return nil
	}
	cp := *g.sim
	return &cp
}

// startConfigPoll launches the owned heartbeat goroutine (idempotent). Called by Connect once the
// initial cadence is known. Does nothing if the enforcer can't do a signed GET (e.g. a test stub).
func (g *Guard) startConfigPoll() {
	getter, ok := g.enforcer.(signedGetter)
	if !ok {
		return
	}
	g.pollMu.Lock()
	if g.pollStarted {
		g.pollMu.Unlock()
		return
	}
	interval := g.pollInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	g.pollStarted = true
	g.pollStop = make(chan struct{})
	g.pollDone = make(chan struct{})
	stop, done := g.pollStop, g.pollDone
	g.pollMu.Unlock()

	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		g.pollOnce(getter) // poll immediately so state is fresh without waiting a full interval
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				g.pollOnce(getter)
			}
		}
	}()
}

// pollOnce does one signed GET /config and applies the result. Poll errors are non-fatal — the
// heartbeat is best-effort observability + dynamic state; a failed tick just retries next interval.
func (g *Guard) pollOnce(getter signedGetter) {
	ctx, cancel := context.WithTimeout(context.Background(), g.cfg.Timeout)
	defer cancel()
	raw, err := getter.GetSigned(ctx, configPath, g.heartbeatHeaders())
	if err != nil {
		return
	}
	var cr flyedgeConfigResponse
	if json.Unmarshal(raw, &cr) != nil {
		return
	}
	g.applyConfig(cr)
}

// applyConfig folds a config response into the Guard: updates model_mode (firing the change
// handler), tracks the simulation block (firing the internal sim hook on change), and honors a
// manifest-refresh request (default: re-Connect).
func (g *Guard) applyConfig(cr flyedgeConfigResponse) {
	if cr.ModelMode != nil {
		nm := ModelMode(*cr.ModelMode)
		g.pollMu.Lock()
		old := g.modelMode
		g.modelMode = nm
		cb := g.onModeChange
		g.pollMu.Unlock()
		if nm != old && cb != nil {
			cb(old, nm)
		}
	}

	simHash := hashSim(cr.Simulation)
	g.pollMu.Lock()
	changed := simHash != g.lastSimHash
	g.lastSimHash = simHash
	g.sim = cr.Simulation
	simCB := g.onSimChange
	g.pollMu.Unlock()
	if changed && simCB != nil {
		simCB(cr.Simulation)
	}

	if cr.ManifestRefreshRequired != nil && *cr.ManifestRefreshRequired {
		g.pollMu.RLock()
		refresh := g.onManifestRefresh
		g.pollMu.RUnlock()
		if refresh != nil {
			refresh()
		} else {
			_ = g.reconnect(context.Background())
		}
	}
}

// heartbeatHeaders builds the X-Agent-* headers prism reads on the config GET: the heartbeat marker
// (refreshes presence TTL), the current manifest hash (drift compare), and the hostname.
func (g *Guard) heartbeatHeaders() map[string]string {
	g.pollMu.RLock()
	mh := g.manifestHash
	g.pollMu.RUnlock()
	return map[string]string{
		"X-Agent-Heartbeat":     "1",
		"X-Agent-Manifest-Hash": mh,
		"X-Agent-Hostname":      g.hostname,
	}
}

// hashSim is a stable content hash of the simulation block, so applyConfig fires the sim hook only
// on real changes (not every heartbeat). nil → "".
func hashSim(s *SimulationConfig) string {
	if s == nil {
		return ""
	}
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
