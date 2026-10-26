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

type InvocationMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type InvocationRequest struct {
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
	// AllowClientState explicitly permits client-state payloads for this handler.
	AllowClientState bool                `json:"allow_client_state"`
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
	// AllowSimulationInjection explicitly permits the existing simulation mutation hooks.
	// Unprotected baseline runs are not supported by remote invocation v1.
	AllowSimulationInjection bool `json:"allow_simulation_injection"`
}

func (o InvocationOptions) validate() (InvocationOptions, error) {
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
	Type      string             `json:"type"`
	ExpiresAt time.Time          `json:"expires_at,omitempty"`
	ID        string             `json:"id,omitempty"`
	Request   *InvocationRequest `json:"request,omitempty"`
	Text      string             `json:"text,omitempty"`
	Error     string             `json:"error,omitempty"`
}
type InvocationConnection struct {
	ID        string
	expiresAt time.Time
	leaseMu   sync.RWMutex
	ws        *websocket.Conn
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	err       error // published before done closes
}

// ExpiresAt returns the latest acknowledged lease expiry. Automatic renewal advances it.
func (c *InvocationConnection) ExpiresAt() time.Time {
	c.leaseMu.RLock()
	defer c.leaseMu.RUnlock()
	return c.expiresAt
}

func (c *InvocationConnection) Wait() error { <-c.done; return c.err }

// Close stops accepting work and signals cancellation. It does not wait for an
// uncooperative handler or undo its external side effects.
func (c *InvocationConnection) Close() error { c.cancel(); _ = c.ws.CloseNow(); <-c.done; return nil }

// ServeInvocations explicitly registers ONE handler and opens an outbound socket.
// Ordinary New/Connect never call it. Leases renew by default until canceled or revoked.
// V1 serves one invocation at a time and
// deliberately does not reconnect/replay after disconnect; call again explicitly
// for a new serving session. Platform revocation is terminal for this lease.
func (g *Guard) ServeInvocations(ctx context.Context, opts InvocationOptions, handler InvocationHandler) (*InvocationConnection, error) {
	opts, err := opts.validate()
	if err != nil {
		return nil, err
	}
	if handler == nil || g.signer == nil {
		return nil, errors.New("flyedge: remote invocation requires a handler and DID signing identity")
	}
	base, err := url.Parse(g.cfg.APIURL)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(base.Hostname())
	if base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Scheme != "https" && !(opts.AllowInsecureLoopback && base.Scheme == "http" && ip != nil && ip.IsLoopback())) {
		return nil, errors.New("flyedge: invocation requires HTTPS (or explicitly allowed literal loopback HTTP)")
	}
	body, _ := json.Marshal(struct {
		InvocationOptions
		Nonce                string `json:"nonce"`
		LeaseDurationSeconds int64  `json:"lease_duration_seconds"`
		TimeoutSeconds       int64  `json:"timeout_seconds"`
	}{opts, randID(), int64(opts.LeaseDuration / time.Second), int64(opts.RequestTimeout / time.Second)})
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
		ID        string    `json:"id"`
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err = json.Unmarshal(raw, &lease); err != nil {
		return nil, err
	}
	if len(lease.ID) != 32 || strings.IndexFunc(lease.ID, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) }) >= 0 || lease.Token == "" || !lease.ExpiresAt.After(time.Now()) || lease.ExpiresAt.After(time.Now().Add(opts.LeaseDuration+5*time.Second)) {
		return nil, errors.New("flyedge: invalid invocation lease")
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
	c := &InvocationConnection{ID: lease.ID, expiresAt: lease.ExpiresAt, ws: ws, ctx: cctx, cancel: cancel, done: make(chan struct{})}
	g.invocationMu.Lock()
	if g.invocationClosed {
		g.invocationMu.Unlock()
		cancel()
		_ = ws.CloseNow()
		return nil, errors.New("flyedge: guard closed")
	}
	g.invocations = append(g.invocations, c)
	g.invocationMu.Unlock()
	go c.serve(g, opts, handler)
	return c, nil
}

type invocationOutput struct {
	mu     sync.Mutex
	c      *InvocationConnection
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
func (c *InvocationConnection) send(ctx context.Context, f invocationFrame) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return wsjson.Write(ctx, c.ws, f)
}
func (c *InvocationConnection) serve(g *Guard, o InvocationOptions, h InvocationHandler) {
	defer close(c.done)
	defer c.cancel()
	defer c.ws.CloseNow()
	type result struct {
		id  string
		err error
		out *invocationOutput
	}
	results := make(chan result, 1)
	frames := make(chan invocationFrame)
	readErr := make(chan error, 1)
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
	var active string
	var cancelWork context.CancelFunc
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
		if cancelWork != nil {
			cancelWork()
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
		case <-readErr:
			c.err = ErrInvocationOutcomeUnknown
			return
		case r := <-results:
			r.out.mu.Lock()
			r.out.closed = true
			r.out.mu.Unlock()
			if cancelWork != nil {
				cancelWork()
			}
			active = ""
			f := invocationFrame{Type: "completed", ID: r.id}
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
				if f.ID == active && cancelWork != nil {
					cancelWork()
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
				} else if r.State != nil && !o.AllowClientState {
					reason = "client state not authorized"
				} else if _, duplicate := seen[r.ID]; duplicate {
					reason = "duplicate invocation"
				} else if active != "" {
					reason = "handler busy"
				} else if !r.Deadline.After(time.Now()) {
					reason = "deadline expired"
				}
				// Simulation permissions never follow from invocation permission. Reject baseline
				// bypass and require the invocation to belong to the active simulation run.
				if g.simCtl != nil && g.simCtl.Active() {
					if g.simCtl.ProtectionDisabled() {
						reason = "unprotected baseline not authorized"
					} else if r.RunID != g.simCtl.RunID() {
						reason = "active simulation belongs to another run"
					} else if !o.AllowSimulationInjection {
						reason = "simulation hooks require explicit permission"
					}
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
				seen[r.ID] = r.Deadline
				deadline := r.Deadline
				if limit := time.Now().Add(o.RequestTimeout); deadline.After(limit) {
					deadline = limit
				}
				work, stop := context.WithDeadline(c.ctx, deadline)
				cancelWork = stop
				active = r.ID
				work = ContextWithSession(work, r.SessionID)
				work = context.WithValue(work, invocationContextKey{}, invocationScope{r.RunID, o.AllowSimulationInjection})
				out := &invocationOutput{c: c, id: r.ID}
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
					select {
					case results <- result{req.ID, err, out}:
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
