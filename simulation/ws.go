package simulation

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	wsInitialBackoff = 1 * time.Second
	wsMaxBackoff     = 30 * time.Second
	wsQueueSize      = 10_000
	wsDialTimeout    = 10 * time.Second
	wsWriteTimeout   = 10 * time.Second
	wsStopGrace      = 5 * time.Second
)

// transport streams JSON RuntimeEvents to the run's telemetry WebSocket
// (Authorization: Bearer <telemetry_jwt>). It owns one goroutine that connects,
// pumps the queue, and reconnects with exponential backoff for the run's lifetime
// (never gives up until Stop). Send is non-blocking and drops on overflow, so
// telemetry never blocks the agent. Mirrors the Python SimulationWsTransport.
type transport struct {
	url string
	jwt string

	queue    chan string
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	sent     atomic.Int64
}

func newTransport(url, jwt string) *transport {
	return &transport{
		url:   url,
		jwt:   jwt,
		queue: make(chan string, wsQueueSize),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
}

// start launches the owned sender goroutine.
func (t *transport) start() { go t.run() }

// send enqueues a JSON event. Non-blocking; drops if the queue is full.
func (t *transport) send(eventJSON string) {
	select {
	case <-t.stop:
	case t.queue <- eventJSON:
	default: // queue full — drop rather than block the agent
	}
}

// stop signals shutdown and waits (bounded) for the goroutine to exit.
func (t *transport) Stop() {
	t.stopOnce.Do(func() { close(t.stop) })
	select {
	case <-t.done:
	case <-time.After(wsStopGrace):
	}
}

func (t *transport) eventsSent() int64 { return t.sent.Load() }

func (t *transport) stopped() bool {
	select {
	case <-t.stop:
		return true
	default:
		return false
	}
}

// sleep waits d, returning false if Stop fired first.
func (t *transport) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-t.stop:
		return false
	case <-timer.C:
		return true
	}
}

func (t *transport) run() {
	defer close(t.done)

	// A context cancelled when Stop fires, so in-flight Dial/Write unblock.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-t.stop
		cancel()
	}()

	hdr := http.Header{"Authorization": []string{"Bearer " + t.jwt}}
	backoff := wsInitialBackoff

	for !t.stopped() {
		dialCtx, dcancel := context.WithTimeout(ctx, wsDialTimeout)
		c, _, err := websocket.Dial(dialCtx, t.url, &websocket.DialOptions{HTTPHeader: hdr})
		dcancel()
		if err != nil {
			if t.stopped() || !t.sleep(backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		backoff = wsInitialBackoff // reset on a successful connect
		err = t.pump(ctx, c)
		_ = c.Close(websocket.StatusNormalClosure, "")
		if t.stopped() {
			return
		}
		// Disconnected unexpectedly (err != nil) — back off and reconnect.
		_ = err
		if !t.sleep(backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

// pump writes queued events to the socket until Stop fires or a write fails
// (returns the error so run reconnects).
func (t *transport) pump(ctx context.Context, c *websocket.Conn) error {
	for {
		select {
		case <-t.stop:
			return nil
		case msg := <-t.queue:
			wctx, wcancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := c.Write(wctx, websocket.MessageText, []byte(msg))
			wcancel()
			if err != nil {
				return err
			}
			t.sent.Add(1)
		}
	}
}

func nextBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > wsMaxBackoff {
		return wsMaxBackoff
	}
	return next
}
