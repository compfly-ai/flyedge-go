package simulation

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Field/list caps matching the Python telemetry middleware (avoid huge WS frames).
const (
	maxFieldChars = 4096
	maxListItems  = 20
)

// Heartbeat cadence (two-phase): a fast burst so the eval-runner subscriber can't
// miss the first connect, then a slow keepalive for liveness.
const (
	hbFastInterval = 5 * time.Second
	hbFastDuration = 60 * time.Second
	hbKeepalive    = 30 * time.Second
)

// Controller manages the simulation lifecycle for a Guard. The config poller calls
// OnConfigChange whenever the `simulation` block changes; the Controller starts/stops
// the telemetry WebSocket, drives the heartbeat, tracks protection_disabled, and
// streams RuntimeEvents the Guard hands it via Record. Mirrors the Python
// SimulationConfigHandler, adapted to Go's explicit interception model (no middleware
// orchestrator — the Guard's own seams call Record).
type Controller struct {
	framework string

	mu           sync.Mutex
	state        State
	runID        string
	protDisabled bool
	emit         bool // "telemetry" middleware enabled
	monitor      bool // "behavior_monitor" middleware enabled
	cfg          *Config
	tr           *transport
	hbStop       chan struct{}
}

// New builds a Controller. framework labels emitted events (e.g. "flyedge-go/anthropic").
func New(framework string) *Controller {
	return &Controller{framework: framework, state: StateInactive}
}

// Active reports whether a simulation run is currently active.
func (c *Controller) Active() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == StateActive
}

// ProtectionDisabled reports whether the active run requested protection be
// disabled (baseline eval) — the Guard then short-circuits /check to allow.
func (c *Controller) ProtectionDisabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state == StateActive && c.protDisabled
}

// RunID returns the active run id ("" if inactive).
func (c *Controller) RunID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runID
}

// OnConfigChange reacts to the simulation block from the config poll. nil ⇒ no
// simulation (deactivate if running). A changed run_id restarts; the same run_id
// hot-swaps config (tier transitions — Phase B2).
func (c *Controller) OnConfigChange(sim *Config) {
	if sim == nil || !sim.Valid() {
		c.mu.Lock()
		running := c.state == StateActive || c.state == StateStarting
		c.mu.Unlock()
		if running {
			c.deactivate()
		}
		return
	}

	c.mu.Lock()
	if c.state == StateActive {
		if sim.RunID == c.runID {
			c.cfg = sim
			c.mu.Unlock()
			c.handleUpdate(sim) // same run — tier hot-swap (Phase B2)
			return
		}
		c.mu.Unlock()
		c.deactivate() // run_id changed — restart
	} else {
		c.mu.Unlock()
	}
	c.activate(sim)
}

// Stop force-deactivates on Guard shutdown.
func (c *Controller) Stop() {
	c.mu.Lock()
	running := c.state != StateInactive
	c.mu.Unlock()
	if running {
		c.deactivate()
	}
}

// handleUpdate handles same-run config changes. Phase B1: just tracks the config
// (Phase B2 will drive attack-injector tier transitions here).
func (c *Controller) handleUpdate(sim *Config) {}

func (c *Controller) activate(sim *Config) {
	tr := newTransport(sim.TelemetryURL, sim.TelemetryJWT)
	tr.start()

	c.mu.Lock()
	c.state = StateActive
	c.runID = sim.RunID
	c.cfg = sim
	c.protDisabled = sim.ProtectionDisabled
	c.emit = sim.HasMiddleware("telemetry")
	c.monitor = sim.HasMiddleware("behavior_monitor")
	c.tr = tr
	c.mu.Unlock()

	c.startHeartbeat(sim)
}

func (c *Controller) deactivate() {
	c.mu.Lock()
	c.state = StateStopping
	tr := c.tr
	hb := c.hbStop
	c.tr = nil
	c.hbStop = nil
	c.mu.Unlock()

	if hb != nil {
		close(hb)
	}
	if tr != nil {
		tr.Stop()
	}

	c.mu.Lock()
	c.state = StateInactive
	c.runID = ""
	c.protDisabled = false
	c.emit = false
	c.monitor = false
	c.cfg = nil
	c.mu.Unlock()
}

// Record finalizes and streams a RuntimeEvent if a run is active and telemetry is
// enabled. It stamps event_id/run_id/timestamp and, when behavior_monitor is on and
// the caller hasn't set flags, attaches behavior flags from bi. No-op otherwise, so
// the Guard can call it unconditionally on every intercepted operation.
func (c *Controller) Record(ev RuntimeEvent, bi BehaviorInput) {
	c.mu.Lock()
	if c.state != StateActive || c.tr == nil || !c.emit {
		c.mu.Unlock()
		return
	}
	ev.RunID = c.runID
	tr := c.tr
	monitor := c.monitor
	c.mu.Unlock()

	ev.EventID = randHex()
	ev.Timestamp = nowUnix()
	if ev.Framework == "" {
		ev.Framework = c.framework
	}
	if monitor && len(ev.Flags) == 0 {
		ev.Flags = Flags(bi)
	}
	if b, err := json.Marshal(&ev); err == nil {
		tr.send(string(b))
	}
}

func (c *Controller) startHeartbeat(sim *Config) {
	stop := make(chan struct{})
	c.mu.Lock()
	c.hbStop = stop
	c.mu.Unlock()

	go func() {
		start := time.Now()
		c.sendHeartbeat(sim)
		for {
			interval := hbKeepalive
			if time.Since(start) < hbFastDuration {
				interval = hbFastInterval
			}
			timer := time.NewTimer(interval)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-timer.C:
				c.sendHeartbeat(sim)
			}
		}
	}()
}

// sendHeartbeat streams a single simulation_connected event so the eval harness
// sees the agent is live and which middlewares are active.
func (c *Controller) sendHeartbeat(sim *Config) {
	c.mu.Lock()
	tr := c.tr
	c.mu.Unlock()
	if tr == nil {
		return
	}
	ev := RuntimeEvent{
		EventID:       randHex(),
		RunID:         sim.RunID,
		PromptID:      SystemPromptID,
		Timestamp:     nowUnix(),
		ComponentType: "simulation",
		ComponentName: "flyedge",
		Framework:     c.framework,
		Flags:         []string{"simulation_connected"},
		ToolArgs: map[string]any{
			"protection_disabled": sim.ProtectionDisabled,
			"middlewares":         sim.Middlewares,
		},
	}
	if b, err := json.Marshal(&ev); err == nil {
		tr.send(string(b))
	}
}

// Truncate caps a string field to maxFieldChars (matches the Python telemetry cap).
func Truncate(s string) string {
	if len(s) > maxFieldChars {
		return s[:maxFieldChars]
	}
	return s
}

// CapList caps a slice of maps to maxListItems (llm_messages / tool_calls).
func CapList(items []map[string]any) []map[string]any {
	if len(items) > maxListItems {
		return items[:maxListItems]
	}
	return items
}

func randHex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
