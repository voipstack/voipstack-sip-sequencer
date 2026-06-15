package b2bua

import (
	"context"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// InboundDialog is the UAS (inbound) side of a call. It snapshots the inbound
// INVITE's relayable headers at accept time (everything not owned by the B2BUA:
// caller From/To, auth, and any custom X-* headers) so every outbound leg can
// reproduce them verbatim and look like the original request to its target. The
// sequencer never copies the inbound Via/Call-ID/CSeq/Max-Forwards/Contact — a
// B2BUA owns those per leg (see requestOwnedHeaders).
type InboundDialog struct {
	session  *sipgo.DialogServerSession
	offerSDP []byte
	headers  []sip.Header
}

// OutboundLeg is one UAC (outbound) leg of a bridged call.
type OutboundLeg struct {
	role      LegRole
	targetURI string
	legID     string
	session   *sipgo.DialogClientSession
	answerSDP []byte
}

// pendingTap holds a tap that has been set up during the app chain but not yet registered
// on MediaSession (which doesn't exist until after PBX 2xx).
type pendingTap struct {
	tap        *Tap
	callerPair PortPair
	calleePair PortPair
}

// Call holds one bridged call: its dialogs, state, and lifecycle context.
type Call struct {
	id              string
	mu              sync.Mutex
	state           CallState
	inbound         InboundDialog
	appLegs         []*OutboundLeg
	pbxLeg          *OutboundLeg
	transferTarget  *sipgo.DialogClientSession // set after a successful REFER transfer
	cancel          context.CancelFunc
	reg             *Registry
	media           *MediaSession
	releaseMedia    func() // closes sockets and releases port pairs; set by bridge
	pendingTaps     []pendingTap
	inboundDetached bool // set after a REFER transfer; inbound dialog-end no longer tears the call down
}

// detachInbound marks the inbound (referrer) leg as intentionally released after a
// successful REFER transfer, so ending its dialog does not tear the whole call down.
// The call continues through the transfer target and PBX legs.
func (c *Call) detachInbound() {
	c.mu.Lock()
	c.inboundDetached = true
	c.mu.Unlock()
}

// inboundTeardownSuppressed reports whether the inbound dialog ending should be
// ignored. After a transfer the inbound leg is detached, so its end is expected.
func (c *Call) inboundTeardownSuppressed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inboundDetached
}

// forwardHeaders returns fresh clones of the snapshotted inbound headers to append
// to an outbound INVITE, making the leg transparent toward its target. Cloning per
// leg keeps each leg's header structs independent (no shared mutable params across
// legs or with sipgo). Supplying From/To explicitly suppresses the UAC's synthesized
// identity; Via/Call-ID/CSeq/Contact/Max-Forwards stay sequencer-owned.
func (c *Call) forwardHeaders() []sip.Header {
	out := make([]sip.Header, 0, len(c.inbound.headers))
	for _, h := range c.inbound.headers {
		out = append(out, sip.HeaderClone(h))
	}
	return out
}

// teardown idempotently shuts down a call: cancels its context, sends BYE on
// every live dialog, and removes it from the registry. Safe to call from multiple
// goroutines simultaneously (glare-safe).
func (c *Call) teardown(reason string) {
	c.mu.Lock()
	if !canTransition(c.state, stateTearingDown) {
		c.mu.Unlock()
		return
	}
	// Only BYE the inbound leg if the call was established: for setup-state calls
	// the bridge goroutine is still sending the final response to inbound, and
	// calling Bye concurrently would race on sipgo's dialog state fields.
	wasEstablished := c.state == stateEstablished
	c.state = stateTearingDown
	// Snapshot session pointers under the lock so bridge cannot race us.
	inbound := c.inbound.session
	var inboundDialogID string
	if inbound != nil {
		inboundDialogID = inbound.ID
	}
	appSessions := make([]*sipgo.DialogClientSession, 0, len(c.appLegs))
	for _, leg := range c.appLegs {
		if leg.session != nil {
			appSessions = append(appSessions, leg.session)
		}
	}
	var pbxSess *sipgo.DialogClientSession
	if c.pbxLeg != nil {
		pbxSess = c.pbxLeg.session
	}
	transferTarget := c.transferTarget
	release := c.releaseMedia
	c.mu.Unlock()

	c.cancel()

	if release != nil {
		release()
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if wasEstablished && inbound != nil {
		_ = inbound.Bye(shutCtx)
	}
	for _, s := range appSessions {
		_ = s.Bye(shutCtx)
	}
	if pbxSess != nil {
		_ = pbxSess.Bye(shutCtx)
	}
	if transferTarget != nil {
		_ = transferTarget.Bye(shutCtx)
	}

	c.reg.remove(c.id, inboundDialogID)
}
