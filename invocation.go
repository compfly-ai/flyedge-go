// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 CompFly AI
package flyedge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type invocationContextKey struct{}
type invocationScope struct {
	runID          string
	allowInjection bool
}

type InvocationPurpose string

const (
	PurposePlayground InvocationPurpose = "playground"
	PurposeSimulation InvocationPurpose = "simulation"
	PurposeEval       InvocationPurpose = "eval"
)

var ErrConnectionExpired = errors.New("flyedge: invocation lease expired")
var ErrConnectionRevoked = errors.New("flyedge: invocation connection revoked")
var ErrInvocationOutcomeUnknown = errors.New("flyedge: invocation connection lost; in-flight outcomes may be unknown")

type InvocationToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}
type InvocationMessage struct {
	Role       string               `json:"role"`
	Content    string               `json:"content"`
	ToolCalls  []InvocationToolCall `json:"toolCalls,omitempty"`
	ToolCallID string               `json:"toolCallId,omitempty"`
}
type InvocationRequest struct {
	ScenarioPlan json.RawMessage `json:"scenario_plan,omitempty"`
	// History is a caller-provided test fixture, separate from live Messages.
	// The handler owns applying it; nil is absent, pointer to empty is explicit empty.
	History         *[]InvocationMessage `json:"history,omitempty"`
	ContractVersion string               `json:"contract_version,omitempty"`
	// State contains client-owned UI/session context, never authorization claims.
	// Nil means absent; a non-nil empty map means an explicitly empty profile.
	State     map[string]json.RawMessage `json:"state,omitempty"`
	ID        string                     `json:"id"`
	Purpose   InvocationPurpose          `json:"purpose"`
	Messages  []InvocationMessage        `json:"messages"`
	SessionID string                     `json:"session_id"`
	RunID     string                     `json:"run_id,omitempty"`
	Deadline  time.Time                  `json:"deadline"`
}

// InvocationOptions grants only the listed purposes for this serving session.
// The handler runs with the application's real credentials. This is not a sandbox.
type InvocationOptions struct {
	// MaxConcurrentInvocations explicitly opts into parallel handlers; zero defaults to one.
	MaxConcurrentInvocations int `json:"max_concurrent_invocations"`
	// AllowClientHistory opts in to test history; defaults to false.
	AllowClientHistory bool `json:"allow_client_history"`
	// Contract is published by the platform before this connection can serve.
	Contract *InvocationContract `json:"contract,omitempty"`
	// AllowClientState defaults to true when nil. Set a pointer to false to reject client state.
	AllowClientState *bool               `json:"allow_client_state"`
	Name             string              `json:"name"`
	Environment      string              `json:"environment"`
	Purposes         []InvocationPurpose `json:"purposes"`
	// LeaseDuration defaults to one hour; renewal keeps the same serving connection alive.
	LeaseDuration time.Duration `json:"-"`
	// AutoRenew defaults to true when nil. Set a pointer to false for a finite session.
	AutoRenew      *bool         `json:"auto_renew,omitempty"`
	RequestTimeout time.Duration `json:"-"`
	// AllowInsecureLoopback permits HTTP only to a literal loopback address for local tests.
	AllowInsecureLoopback bool `json:"-"`
	// AllowOperatorReconnect lets a platform operator request a fresh lease after a disconnect.
	AllowOperatorReconnect bool `json:"-"`
	// AllowSimulationInjection defaults to true when nil for reverse invocation.
	// Set a pointer to false to reject injected mocks.
	AllowSimulationInjection *bool `json:"allow_simulation_injection"`
}

func (o InvocationOptions) validate() (InvocationOptions, error) {
	if o.MaxConcurrentInvocations == 0 {
		o.MaxConcurrentInvocations = 1
	}
	if o.MaxConcurrentInvocations < 1 || o.MaxConcurrentInvocations > 64 {
		return o, errors.New("flyedge: invocation concurrency must be 1–64")
	}
	var err error
	o.Contract, err = snapshotInvocationContract(o.Contract)
	if err != nil {
		return o, err
	}
	allowInjection := true
	if o.AllowSimulationInjection != nil {
		allowInjection = *o.AllowSimulationInjection
	}
	o.AllowSimulationInjection = &allowInjection
	allowState := true
	if o.AllowClientState != nil {
		allowState = *o.AllowClientState
	}
	o.AllowClientState = &allowState
	if o.Contract != nil && len(o.Contract.State.Fields) > 0 && !*o.AllowClientState {
		return o, errors.New("flyedge: a state contract requires AllowClientState")
	}
	if o.LeaseDuration == 0 {
		o.LeaseDuration = time.Hour
	}
	renew := true
	if o.AutoRenew != nil {
		renew = *o.AutoRenew
	}
	o.AutoRenew = &renew
	if o.Environment == "" || len(o.Environment) > 64 || len(o.Name) > 128 || len(o.Purposes) == 0 || o.LeaseDuration < time.Second || o.LeaseDuration > 24*time.Hour {
		return o, errors.New("flyedge: environment, purposes and a valid lease duration (1s–24h) are required")
	}
	for _, p := range o.Purposes {
		if p != PurposePlayground && p != PurposeSimulation && p != PurposeEval {
			return o, fmt.Errorf("flyedge: invalid invocation purpose %q", p)
		}
	}
	o.Purposes = slices.Clone(o.Purposes)
	if o.RequestTimeout == 0 {
		o.RequestTimeout = 2 * time.Minute
	}
	if o.RequestTimeout < time.Second || o.RequestTimeout > 10*time.Minute {
		return o, errors.New("flyedge: request timeout must be 1s–10m")
	}
	return o, nil
}

// InvocationWriter streams text to Platform. Returning nil from the handler completes
// the response; returning an error fails it. Calls after handler completion fail.
type InvocationWriter interface {
	TextDelta(context.Context, string) error
}
type InvocationHandler func(context.Context, InvocationRequest, InvocationWriter) error

type invocationFrame struct {
	ScenarioSnapshot map[string]any     `json:"scenario_snapshot,omitempty"`
	Type             string             `json:"type"`
	ExpiresAt        time.Time          `json:"expires_at,omitempty"`
	ID               string             `json:"id,omitempty"`
	Request          *InvocationRequest `json:"request,omitempty"`
	Text             string             `json:"text,omitempty"`
	Error            string             `json:"error,omitempty"`
}

// InvocationConnection represents a serving session. ID and ContractVersion describe
// its initial lease. Use ActiveLeaseID to identify the currently connected lease.
type InvocationConnection struct {
	ContractVersion string
	ID              string
	environment     string
	reconnect       chan string
	cancel          context.CancelFunc
	done            chan struct{}
	err             error // published before done closes
	mu              sync.RWMutex
	lease           *invocationLease
	connected       bool
}

type invocationLease struct {
	contractVersion string
	id              string
	expiresAt       time.Time
	leaseMu         sync.RWMutex
	ws              *websocket.Conn
	ctx             context.Context
	cancel          context.CancelFunc
	err             error
}

// ExpiresAt returns the latest acknowledged lease expiry. Automatic renewal advances it.
func (c *InvocationConnection) ExpiresAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lease.ExpiresAt()
}

// ActiveLeaseID returns the current lease ID, or an empty string when disconnected.
func (c *InvocationConnection) ActiveLeaseID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if !c.connected {
		return ""
	}
	return c.lease.id
}

func (c *invocationLease) ExpiresAt() time.Time {
	c.leaseMu.RLock()
	defer c.leaseMu.RUnlock()
	return c.expiresAt
}

func (c *InvocationConnection) Wait() error { <-c.done; return c.err }

// Close stops accepting work and signals cancellation. It does not wait for an
// uncooperative handler or undo its external side effects.
func (c *InvocationConnection) Close() error { c.cancel(); <-c.done; return nil }

// ServeInvocations explicitly registers ONE handler and opens an outbound socket.
// Ordinary New/Connect never call it. Leases renew by default until canceled or revoked.
// Handlers run up to MaxConcurrentInvocations at a time. A disconnect ends the
// session unless AllowOperatorReconnect is set. With opt-in, a platform operator
// can request a new lease until the caller closes or cancels the session.
func (g *Guard) ServeInvocations(ctx context.Context, opts InvocationOptions, handler InvocationHandler) (*InvocationConnection, error) {
	opts, err := opts.validate()
	if err != nil {
		return nil, err
	}
	if handler == nil || g.signer == nil {
		return nil, errors.New("flyedge: remote invocation requires a handler and DID signing identity")
	}
	if _, ok := g.enforcer.(signedGetter); opts.AllowOperatorReconnect && !ok {
		return nil, errors.New("flyedge: operator reconnect requires signed config heartbeats")
	}
	ctx, cancel := context.WithCancel(ctx)
	lease, err := g.openInvocation(ctx, opts, "")
	if err != nil {
		cancel()
		return nil, err
	}
	c := &InvocationConnection{
		ContractVersion: lease.contractVersion, ID: lease.id, environment: opts.Environment,
		lease: lease, connected: true, cancel: cancel, done: make(chan struct{}),
	}
	if opts.AllowOperatorReconnect {
		c.reconnect = make(chan string, 1)
	}
	if err := g.trackInvocation(c); err != nil {
		cancel()
		_ = lease.ws.CloseNow()
		return nil, err
	}
	go c.serve(ctx, g, opts, handler)
	return c, nil
}

func (g *Guard) trackInvocation(c *InvocationConnection) error {
	g.invocationMu.Lock()
	defer g.invocationMu.Unlock()
	if g.invocationClosed {
		return errors.New("flyedge: guard closed")
	}
	if c.reconnect != nil {
		count := 0
		for _, other := range g.invocations {
			if other.reconnect == nil {
				continue
			}
			if other.environment == c.environment {
				return errors.New("flyedge: operator reconnect is already enabled for this environment")
			}
			count++
		}
		if count == 8 {
			return errors.New("flyedge: operator reconnect supports at most eight environments per guard")
		}
		g.startConfigPoll()
	}
	g.invocations = append(g.invocations, c)
	return nil
}

func (c *InvocationConnection) serve(ctx context.Context, g *Guard, opts InvocationOptions, handler InvocationHandler) {
	defer close(c.done)
	defer c.cancel()
	defer func() {
		g.invocationMu.Lock()
		g.invocations = slices.DeleteFunc(g.invocations, func(other *InvocationConnection) bool { return other == c })
		g.invocationMu.Unlock()
	}()

	lease := c.lease
	lastAttempt := ""
	for {
		if lease != nil {
			lease.serve(g, opts, handler)
			c.mu.Lock()
			c.connected = false
			c.mu.Unlock()
			if c.reconnect == nil {
				c.err = lease.err
				return
			}
			lease = nil
		}

		select {
		case <-ctx.Done():
			c.err = ctx.Err()
			return
		case request := <-c.reconnect:
			if request == "" || request == lastAttempt || ctx.Err() != nil {
				continue
			}
			lastAttempt = request
			next, err := g.openInvocation(ctx, opts, request)
			if err == nil {
				lease = next
				c.mu.Lock()
				c.lease, c.connected = lease, true
				c.mu.Unlock()
			}
		}
	}
}

func (g *Guard) openInvocation(ctx context.Context, opts InvocationOptions, reconnectRequestID string) (*invocationLease, error) {
	base, err := url.Parse(g.cfg.APIURL)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(base.Hostname())
	if base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "https" && !(opts.AllowInsecureLoopback && base.Scheme == "http" && ip != nil && ip.IsLoopback())) {
		return nil, errors.New("flyedge: invocation requires HTTPS (or explicitly allowed literal loopback HTTP)")
	}
	body, err := json.Marshal(struct {
		InvocationOptions
		ReconnectRequestID       string `json:"reconnect_request_id,omitempty"`
		ScenarioExecutionVersion int    `json:"scenario_execution_version"`
		Nonce                    string `json:"nonce"`
		LeaseDurationSeconds     int64  `json:"lease_duration_seconds"`
		TimeoutSeconds           int64  `json:"timeout_seconds"`
	}{
		InvocationOptions: opts, ReconnectRequestID: reconnectRequestID, ScenarioExecutionVersion: 2,
		Nonce: randID(), LeaseDurationSeconds: int64(opts.LeaseDuration / time.Second), TimeoutSeconds: int64(opts.RequestTimeout / time.Second),
	})
	if err != nil {
		return nil, err
	}
	registration, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(g.cfg.APIURL, "/")+"/v1/flyedge/invocations", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	headers, err := g.signer.Sign(body, time.Now())
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		registration.Header.Set(k, v)
	}
	registration.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: g.cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(registration)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnprocessableEntity {
		return nil, errors.New("flyedge: invocation registration rejected (HTTP 422); check the registration options and ensure the broker supports LeaseDuration and automatic renewal (a broker rollout may still be in progress)")
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("flyedge: invocation registration returned HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16*1024))
	if err != nil {
		return nil, err
	}
	var lease struct {
		ContractVersion string    `json:"contract_version"`
		ID              string    `json:"id"`
		Token           string    `json:"token"`
		ExpiresAt       time.Time `json:"expires_at"`
	}
	if err = json.Unmarshal(raw, &lease); err != nil {
		return nil, err
	}
	if len(lease.ID) != 32 || strings.IndexFunc(lease.ID, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) }) >= 0 || lease.Token == "" || !lease.ExpiresAt.After(time.Now()) || lease.ExpiresAt.After(time.Now().Add(opts.LeaseDuration+5*time.Second)) {
		return nil, errors.New("flyedge: invalid invocation lease")
	}
	if opts.Contract != nil && (len(lease.ContractVersion) != 71 || !strings.HasPrefix(lease.ContractVersion, "sha256:")) {
		return nil, errors.New("flyedge: broker did not acknowledge the invocation contract")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/v1/flyedge/invocations/" + lease.ID + "/socket"
	if base.Scheme == "https" {
		base.Scheme = "wss"
	} else {
		base.Scheme = "ws"
	}
	cctx, cancel := context.WithCancel(ctx)
	dialCtx, stopDial := context.WithTimeout(cctx, g.cfg.Timeout)
	ws, _, err := websocket.Dial(dialCtx, base.String(), &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + lease.Token}}, HTTPClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}})
	stopDial()
	if err != nil {
		cancel()
		return nil, err
	}
	ws.SetReadLimit(256 * 1024)
	readyCtx, stopReady := context.WithTimeout(cctx, g.cfg.Timeout)
	var ready invocationFrame
	err = wsjson.Read(readyCtx, ws, &ready)
	stopReady()
	if err != nil || ready.Type != "ready" {
		cancel()
		_ = ws.CloseNow()
		return nil, errors.New("flyedge: invocation connection did not become ready")
	}
	return &invocationLease{contractVersion: lease.ContractVersion, id: lease.ID, expiresAt: lease.ExpiresAt, ws: ws, ctx: cctx, cancel: cancel}, nil
}

type invocationOutput struct {
	mu     sync.Mutex
	c      *invocationLease
	id     string
	closed bool
	size   int
}

func (w *invocationOutput) TextDelta(ctx context.Context, text string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("flyedge: invocation finished")
	}
	if w.size+len(text) > 1024*1024 {
		return errors.New("flyedge: invocation output exceeds 1 MiB")
	}
	w.size += len(text)
	return w.c.send(ctx, invocationFrame{Type: "delta", ID: w.id, Text: text})
}
func (c *invocationLease) send(ctx context.Context, f invocationFrame) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(ctx, c.ws, f)
}

// invocationHeartbeat keeps the invocation socket active independently of the
// HTTP config poller and lease renewal. Ping handles matching pongs; serve's
// reader must remain active while it waits. Each ping has its own deadline.
func invocationHeartbeat(ctx context.Context, ws *websocket.Conn, interval, timeout time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, timeout)
			err := ws.Ping(pingCtx)
			cancel()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err != nil {
				return err
			}
		}
	}
}

func (c *invocationLease) serve(g *Guard, o InvocationOptions, h InvocationHandler) {
	defer c.cancel()
	defer c.ws.CloseNow()
	type result struct {
		snapshot map[string]any
		id       string
		err      error
		out      *invocationOutput
	}
	results := make(chan result, o.MaxConcurrentInvocations)
	frames := make(chan invocationFrame)
	readErr := make(chan error, 1)
	heartbeatErr := make(chan error, 1)
	go func() {
		heartbeatErr <- invocationHeartbeat(c.ctx, c.ws, 30*time.Second, 30*time.Second)
	}()
	go func() {
		for {
			var f invocationFrame
			if err := wsjson.Read(c.ctx, c.ws, &f); err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- f:
			case <-c.ctx.Done():
				return
			}
		}
	}()
	type activeWork struct {
		cancel  context.CancelFunc
		session string
	}
	active := map[string]activeWork{}
	sessions := map[string]bool{}
	seen := map[string]time.Time{}
	expiry := time.NewTimer(time.Until(c.ExpiresAt()))
	defer expiry.Stop()
	renewal := time.NewTimer(time.Until(c.ExpiresAt()) / 2)
	defer renewal.Stop()
	var renewAt <-chan time.Time
	if *o.AutoRenew {
		renewAt = renewal.C
	}
	pendingRenewal := ""
	defer func() {
		for _, work := range active {
			work.cancel()
		}
	}()
	for {
		select {
		case <-expiry.C:
			c.err = ErrConnectionExpired
			return
		case <-renewAt:
			pendingRenewal = randID()
			deadline, stop := context.WithDeadline(c.ctx, c.ExpiresAt())
			err := c.send(deadline, invocationFrame{Type: "renew", ID: pendingRenewal})
			stop()
			if err != nil {
				c.err = ErrInvocationOutcomeUnknown
				return
			}
			// Never extend locally without the broker's acknowledgement. Missing ack expires.
			renewAt = nil
		case <-c.ctx.Done():
			c.err = c.ctx.Err()
			return
		case err := <-heartbeatErr:
			if c.ctx.Err() != nil {
				c.err = c.ctx.Err()
			} else {
				c.err = fmt.Errorf("%w: websocket heartbeat: %w", ErrInvocationOutcomeUnknown, err)
			}
			return
		case err := <-readErr:
			c.err = fmt.Errorf("%w: websocket read: %w", ErrInvocationOutcomeUnknown, err)
			return
		case r := <-results:
			r.out.mu.Lock()
			r.out.closed = true
			r.out.mu.Unlock()
			if work, ok := active[r.id]; ok {
				work.cancel()
				delete(sessions, work.session)
				delete(active, r.id)
			}
			f := invocationFrame{Type: "completed", ID: r.id, ScenarioSnapshot: r.snapshot}
			if r.err != nil {
				f.Type = "failed"
				f.Error = r.err.Error()
			}
			if c.send(c.ctx, f) != nil {
				c.err = ErrInvocationOutcomeUnknown
				return
			}
		case f := <-frames:
			switch f.Type {
			case "renewed":
				if pendingRenewal == "" || f.ID != pendingRenewal || !f.ExpiresAt.After(c.ExpiresAt()) || f.ExpiresAt.After(time.Now().Add(o.LeaseDuration+5*time.Second)) || !time.Now().Before(c.ExpiresAt()) {
					c.err = errors.New("flyedge: invalid lease renewal acknowledgement")
					return
				}
				c.leaseMu.Lock()
				c.expiresAt = f.ExpiresAt
				c.leaseMu.Unlock()
				expiry.Reset(time.Until(f.ExpiresAt))
				renewal.Reset(time.Until(f.ExpiresAt) / 2)
				renewAt = renewal.C
				pendingRenewal = ""
			case "expired":
				c.err = ErrConnectionExpired
				return
			case "revoked":
				c.err = ErrConnectionRevoked
				return
			case "cancel":
				if work, ok := active[f.ID]; ok {
					work.cancel()
				}
			case "invoke":
				if !time.Now().Before(c.ExpiresAt()) {
					c.err = ErrConnectionExpired
					return
				}
				r := f.Request
				if r == nil || r.ID == "" || len(r.ID) > 128 {
					c.err = errors.New("flyedge: malformed invocation")
					return
				}
				reason := ""
				permitted := false
				for _, p := range o.Purposes {
					if r.Purpose == p {
						permitted = true
					}
				}
				if !permitted {
					reason = "purpose not authorized"
				} else if c.contractVersion != "" && r.ContractVersion != c.contractVersion {
					reason = "invocation contract version mismatch"
				} else if r.State != nil && !*o.AllowClientState {
					reason = "client state not authorized"
				} else if err := validateInvocationHistory(r.History, o.AllowClientHistory); err != nil {
					reason = err.Error()
				} else if _, duplicate := seen[r.ID]; duplicate {
					reason = "duplicate invocation"
				} else if len(active) >= o.MaxConcurrentInvocations {
					reason = "handler busy"
				} else if sessions[r.SessionID] {
					reason = "session busy"
				} else if !r.Deadline.After(time.Now()) {
					reason = "deadline expired"
				}
				// Simulation permissions never follow from invocation permission. Reject baseline
				// bypass and require the invocation to belong to the active simulation run.
				if reason == "" && g.simCtl != nil {
					reason = g.simCtl.InvocationRejection(r.RunID, *o.AllowSimulationInjection)
				}
				if reason != "" {
					if c.send(c.ctx, invocationFrame{Type: "failed", ID: r.ID, Error: reason}) != nil {
						c.err = ErrInvocationOutcomeUnknown
						return
					}
					continue
				}
				// Expired request IDs cannot be replayed because their deadline is checked above.
				if len(seen) >= 10000 {
					for id, deadline := range seen {
						if !deadline.After(time.Now()) {
							delete(seen, id)
						}
					}
				}
				if len(seen) >= 10000 {
					if c.send(c.ctx, invocationFrame{Type: "failed", ID: r.ID, Error: "invocation replay window full"}) != nil {
						c.err = ErrInvocationOutcomeUnknown
						return
					}
					continue
				}
				if len(r.ScenarioPlan) > 0 {
					if (!*o.AllowSimulationInjection && scenarioPlanRequiresInjection(r.ScenarioPlan)) || r.RunID == "" || !strings.HasPrefix(r.SessionID, "eval:"+r.RunID+":") || g.simCtl == nil {
						reason = "scenario plan requires authorized simulation scope"
					} else if err := g.simCtl.BeginExecution(r.SessionID, r.ScenarioPlan); err != nil {
						reason = err.Error()
					}
					if reason != "" {
						if c.send(c.ctx, invocationFrame{Type: "failed", ID: r.ID, Error: reason}) != nil {
							c.err = ErrInvocationOutcomeUnknown
							return
						}
						continue
					}
				}
				seen[r.ID] = r.Deadline
				deadline := r.Deadline
				if limit := time.Now().Add(o.RequestTimeout); deadline.After(limit) {
					deadline = limit
				}
				work, stop := context.WithDeadline(c.ctx, deadline)
				active[r.ID] = activeWork{stop, r.SessionID}
				sessions[r.SessionID] = true
				work = ContextWithSession(work, r.SessionID)
				work = context.WithValue(work, invocationContextKey{}, invocationScope{r.RunID, *o.AllowSimulationInjection})
				out := &invocationOutput{c: c, id: r.ID}
				planID := ""
				if g.simCtl != nil {
					planID = g.simCtl.ScenarioToken()
				}
				go func(req InvocationRequest) {
					var err error
					func() {
						defer func() {
							if recover() != nil {
								err = errors.New("agent handler panicked")
							}
						}()
						err = h(work, req, out)
					}()
					if err == nil {
						err = work.Err()
					}
					var snapshot map[string]any
					if g.simCtl != nil && len(req.ScenarioPlan) > 0 {
						snapshot = g.simCtl.EndExecution(req.SessionID)
						if snapshot != nil {
							snapshot["handler_success"] = err == nil
						}
					} else if g.simCtl != nil {
						g.simCtl.FinishScenario(req.RunID, req.SessionID, planID, err == nil)
					}
					select {
					case results <- result{snapshot, req.ID, err, out}:
					case <-c.ctx.Done():
					}
				}(*r)
			default:
				c.err = errors.New("flyedge: unexpected invocation frame")
				return
			}
		}
	}
}

func validateInvocationHistory(history *[]InvocationMessage, allowed bool) error {
	if history == nil {
		return nil
	}
	if !allowed {
		return errors.New("client history not authorized")
	}
	data, err := json.Marshal(*history)
	if err != nil {
		return err
	}
	var messages []any
	if err := json.Unmarshal(data, &messages); err != nil {
		return err
	}
	issues := validateHistoryMessages(messages)
	if len(issues) > 0 {
		return errors.New(issues[0].Message)
	}
	return nil
}

func scenarioPlanRequiresInjection(raw json.RawMessage) bool {
	var p struct {
		Overrides []struct {
			Enabled *bool `json:"enabled"`
		} `json:"overrides"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return true
	}
	for _, r := range p.Overrides {
		if r.Enabled == nil || *r.Enabled {
			return true
		}
	}
	return false
}
