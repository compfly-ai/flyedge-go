package flyedge

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"time"
)

// InvocationHTTPError preserves a broker rejection without retaining its body or credentials.
type InvocationHTTPError struct {
	StatusCode int
	RetryAfter time.Duration
}

func (e *InvocationHTTPError) Error() string {
	return fmt.Sprintf("flyedge: invocation request returned HTTP %d", e.StatusCode)
}
func invocationHTTPError(r *http.Response) error {
	d := time.Duration(0)
	if seconds, err := strconv.Atoi(r.Header.Get("Retry-After")); err == nil && seconds > 0 {
		d = time.Duration(min(seconds, 86400)) * time.Second
	} else if at, err := http.ParseTime(r.Header.Get("Retry-After")); err == nil {
		d = max(0, time.Until(at))
	}
	return &InvocationHTTPError{StatusCode: r.StatusCode, RetryAfter: d}
}
func retryInvocationRecovery(err error) bool {
	var h *InvocationHTTPError
	if errors.As(err, &h) {
		return h.StatusCode == 409 || h.StatusCode == 429 || h.StatusCode >= 500 && h.StatusCode <= 599
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var n net.Error
	return errors.As(err, &n) || errors.Is(err, ErrInvocationOutcomeUnknown)
}
func invocationRecoveryDelay(attempt uint64) time.Duration {
	cap := time.Second
	for i := uint64(1); i < attempt && cap < 30*time.Second; i++ {
		cap = min(cap*2, 30*time.Second)
	}
	return cap/2 + time.Duration(rand.Int64N(int64(cap/2)+1))
}
func invocationRecoveryRetryDelay(attempt uint64, last error) time.Duration {
	var h *InvocationHTTPError
	if errors.As(last, &h) && h.StatusCode == http.StatusConflict {
		return invocationRecoveryDelay(1)
	}
	return invocationRecoveryDelay(attempt)
}
func (c *InvocationConnection) recover(ctx context.Context, g *Guard, o InvocationOptions, previous *invocationLease) (*invocationLease, error) {
	sessionCtx := ctx
	ctx, cancel := context.WithDeadline(ctx, previous.ExpiresAt())
	defer cancel()
	var last error
	for attempt := uint64(1); ; attempt++ {
		delay := invocationRecoveryRetryDelay(attempt, last)
		var h *InvocationHTTPError
		if errors.As(last, &h) {
			delay = max(delay, h.RetryAfter)
		}
		// The lease deadline also bounds Retry-After and prevents a late new attempt.
		c.mu.Lock()
		c.state = InvocationReconnecting
		c.nextRetryAt = time.Now().Add(delay)
		c.mu.Unlock()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrConnectionExpired
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrConnectionExpired
			}
			return nil, ctx.Err()
		}
		c.mu.Lock()
		c.nextRetryAt = time.Time{}
		c.recoveryAttempts++
		c.mu.Unlock()
		next, err := g.openInvocationRequest(sessionCtx, o, "", previous)
		if err == nil {
			return next, nil
		}
		last = err
		c.mu.Lock()
		c.lastReconnectError = err
		c.mu.Unlock()
		if !retryInvocationRecovery(err) {
			return nil, err
		}
	}
}

// InvocationHeartbeat contains transport timing only, never invocation payloads.
// LastFrameAt counts application frames; the library handles control frames internally.
type InvocationHeartbeat struct {
	EdgeRay       string // Cloudflare handshake trace ID, when provided by the endpoint.
	LastPingAt    time.Time
	LastPongAt    time.Time
	LastFrameAt   time.Time
	PingsSent     uint64
	PongsReceived uint64
}
