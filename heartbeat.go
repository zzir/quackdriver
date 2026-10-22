package quackdriver

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/zzir/quackdriver/internal/message"
)

const (
	// maxHeartbeatRequestTimeout bounds one HEARTBEAT_REQUEST. A request
	// that hangs (a pooled TCP connection silently dropped by a middlebox)
	// must give up well within the lease, so that a retry can still renew it.
	maxHeartbeatRequestTimeout = 10 * time.Second
	// maxHeartbeatRetryDelay bounds the pause before a heartbeat that failed
	// in transit is retried (on a fresh HTTP connection).
	maxHeartbeatRetryDelay = time.Second
)

// heartbeatIntervalOverride, when non-zero, replaces the computed interval
// (tests use it to run faster than whole-second leases allow).
var heartbeatIntervalOverride atomic.Int64

// heartbeatInterval returns the pause before the next heartbeat: a third of
// the lease, with ±20% jitter, like the official client (HeartbeatInterval in
// quack_client.cpp).
func heartbeatInterval(lease time.Duration) time.Duration {
	if d := heartbeatIntervalOverride.Load(); d > 0 {
		return time.Duration(d)
	}
	d := time.Duration(float64(lease) / 3 * (0.8 + 0.4*rand.Float64()))
	if d <= 0 {
		d = time.Millisecond
	}
	return d
}

// heartbeatRequestTimeout bounds one heartbeat request: a third of the
// lease, at most maxHeartbeatRequestTimeout.
func heartbeatRequestTimeout(lease time.Duration) time.Duration {
	d := min(lease/3, maxHeartbeatRequestTimeout)
	if d <= 0 {
		d = time.Millisecond
	}
	return d
}

// startHeartbeat starts the goroutine that renews the session's lease. The
// server renews a lease only when a message arrives and expires the session
// once a full lease passes without one, so heartbeats are sent even while a
// statement runs: they travel on their own HTTP connection from the shared
// pool.
func (c *Conn) startHeartbeat() {
	ctx, cancel := context.WithCancel(context.Background())
	c.hbCancel = cancel
	c.hbDone = make(chan struct{})
	go c.heartbeatLoop(ctx, c.hbDone, c.serverInfo.Lease)
}

// stopHeartbeat stops the heartbeat goroutine and waits for it to exit.
func (c *Conn) stopHeartbeat() {
	if c.hbCancel == nil {
		return
	}
	c.hbCancel()
	<-c.hbDone
}

func (c *Conn) heartbeatLoop(ctx context.Context, done chan<- struct{}, lease time.Duration) {
	defer close(done)
	timer := time.NewTimer(heartbeatInterval(lease))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if c.closed.Load() || c.bad.Load() {
			return
		}
		next := c.heartbeat(ctx, lease)
		if next == 0 {
			return
		}
		timer.Reset(next)
	}
}

// heartbeat sends one HEARTBEAT_REQUEST, bounded by heartbeatRequestTimeout,
// and returns the pause before the next one, or 0 once the session is gone
// (the connection is then marked gone). Failures are retried: soon after a
// failure in transit, at the next tick after an error answer. Once a whole
// lease has passed without any answer from the server (c.lastOK), the
// server has expired the session: the connection is marked gone instead of
// dialing an unreachable server forever.
func (c *Conn) heartbeat(ctx context.Context, lease time.Duration) time.Duration {
	rctx, cancel := context.WithTimeout(ctx, heartbeatRequestTimeout(lease))
	defer cancel()
	_, err := c.send(rctx, 0, message.HeartbeatRequest{})
	var se *Error
	switch {
	case err == nil:
		c.heartbeats.Add(1)
	case !errors.As(err, &se):
		if time.Since(time.Unix(0, c.lastOK.Load())) > lease {
			c.markGone()
			return 0
		}
		return min(heartbeatInterval(lease), maxHeartbeatRetryDelay)
	case isConnGone(se) || se.MustInvalidate:
		c.markGone()
		return 0
	}
	return heartbeatInterval(lease)
}
