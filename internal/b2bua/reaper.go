package b2bua

import (
	"context"
	"log/slog"
	"time"
)

// reaperMaxInterval caps how often the idle sweep runs, so a long idle_timeout still
// reaps reasonably promptly without the sweep ever spinning too fast.
const reaperMaxInterval = 30 * time.Second

// startReaper runs a periodic sweep that tears down established calls whose media has
// gone idle longer than e.idleTimeout — reclaiming RTP ports and relay goroutines when an
// endpoint or PBX disappears without a BYE. It stops when ctx is cancelled. The caller
// starts it only when idleTimeout > 0.
func (e *Engine) startReaper(ctx context.Context) {
	interval := e.idleTimeout / 2
	if interval > reaperMaxInterval {
		interval = reaperMaxInterval
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				e.calls.each(func(c *Call) {
					if c.idleExpired(now, e.idleTimeout) {
						slog.Warn("call torn down: media idle past rtp.idle_timeout",
							"call", c.id, "idle_timeout", e.idleTimeout)
						c.teardown("media inactivity timeout")
					}
				})
			}
		}
	}()
}

// idleExpired reports whether the call is an established media call that has exchanged no
// RTP or RTCP (in either direction) for longer than timeout. Setup-state calls (bounded by
// their own leg deadlines) and calls without a media session are never reaped.
func (c *Call) idleExpired(now time.Time, timeout time.Duration) bool {
	c.mu.Lock()
	established := c.state == stateEstablished
	media := c.media
	c.mu.Unlock()
	if !established || media == nil {
		return false
	}
	return media.idleFor(now) > timeout
}
