package b2bua

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// withTransport returns u with its transport URI parameter forced to the given value,
// overriding any existing one. It does not mutate u's params (the clone is fresh).
func withTransport(u sip.Uri, transport string) sip.Uri {
	params := u.UriParams.Clone()
	params.Add("transport", transport)
	u.UriParams = params
	return u
}

// withTCP forces transport=tcp. Plain application legs always run over TCP because the
// offer (grown in tap mode) can exceed sipgo's UDP MTU guard.
func withTCP(u sip.Uri) sip.Uri {
	return withTransport(u, "tcp")
}

// dialContext bounds the dial — where sipgo performs the TCP+TLS connect — by the
// profile's connect_timeout when one is set, so a dead TLS peer fails fast instead of
// hanging the call. A zero timeout (or nil profile) leaves the dial unbounded. The
// caller always invokes the returned cancel once the dial returns, before answer wait.
func dialContext(parent context.Context, rp *config.ResolvedTLSProfile) (context.Context, context.CancelFunc) {
	if rp != nil && rp.ConnectTimeout > 0 {
		return context.WithTimeout(parent, rp.ConnectTimeout)
	}
	return parent, func() {}
}

// handleInvite is the INVITE handler. It accepts the inbound dialog, creates a
// Call, and runs the bridge synchronously.
//
// It MUST NOT return until a final response is sent to the inbound caller.
// sipgo's Server.handleRequest calls tx.TerminateGracefully() after this
// function returns; if no final response was sent yet, TerminateGracefully
// calls tx.Terminate() immediately, which fires OnTerminate → endWithCause →
// DialogStateEnded → teardown — cancelling callCtx before the bridge can send
// anything. Running bridge() synchronously here prevents that race.
func (e *Engine) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	// An inbound request whose top Route is this sequencer's Path with a valid flow
	// token is routed back to the webphone over its existing flow, not bridged as a
	// new caller. A normal caller INVITE has no self-Route and is unaffected.
	if e.routeToFlow(req, tx) {
		return
	}

	// Check for an in-dialog re-INVITE before touching the initial-INVITE path.
	if existingDSS, matchErr := e.dialogSrvCache.MatchDialogRequest(req); matchErr == nil {
		call, ok := e.calls.getByDialog(existingDSS.ID)
		if !ok {
			_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil))
			return
		}
		e.handleReInvite(call, existingDSS, req, tx)
		return
	}

	dss, err := e.dialogSrvCache.ReadInvite(req, tx)
	if err != nil {
		slog.Error("accept inbound INVITE", "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Server Error", nil))
		return
	}

	if err := dss.Respond(100, "Trying", nil); err != nil {
		slog.Warn("send 100 Trying", "err", err)
	}

	callCtx, cancel := context.WithCancel(e.runCtx)
	call := &Call{
		id:     newCallID(),
		state:  stateSetup,
		cancel: cancel,
		reg:    e.calls,
		inbound: InboundDialog{
			session:  dss,
			offerSDP: copyBody(req.Body()),
			headers:  relayableHeaders(req, requestOwnedHeaders),
		},
	}
	e.calls.add(call)
	e.calls.addDialog(dss.ID, call)

	dss.OnState(func(s sip.DialogState) {
		if s == sip.DialogStateEnded {
			call.teardown("inbound dialog ended")
		}
	})

	// Run synchronously: the handler goroutine must stay alive until bridge
	// sends a final response so that TerminateGracefully sees finalized=true.
	e.bridge(callCtx, call)
}

// bridge sequences one call through the three setup phases, then runs media relay
// until the call ends. Each phase returns false when it has already sent a final
// response and torn the call down, in which case bridge stops immediately.
//
// All sipgo I/O lives in these phases; pure helpers (mapFailureStatus,
// canTransition) live in state.go.
func (e *Engine) bridge(ctx context.Context, call *Call) {
	start := time.Now()

	if !e.runAppChain(ctx, call) {
		return
	}
	anchor, ok := e.anchorMedia(call)
	if !ok {
		return
	}
	if !e.dialPBX(ctx, call, anchor, start) {
		return
	}

	go anchor.session.relay(ctx)
	<-ctx.Done()
}

// fail sends a final response to the inbound caller, releases any taps stashed on
// the call before the media session took ownership of them, and tears the call
// down. Pending taps are only reachable here until dialPBX registers them on the
// media session (success path), so every setup-phase failure must release them.
func (e *Engine) fail(call *Call, status int, reason, cause string) {
	_ = call.inbound.session.Respond(status, reason, nil)
	releasePendingTaps(call, e.ports)
	call.teardown(cause)
}

// tapResources holds the two port pairs and bound anchor sides for one tap.
type tapResources struct {
	callerPair PortPair
	calleePair PortPair
	callerSide *AnchorSide
	calleeSide *AnchorSide
}

// release closes both sides and returns both pairs to the allocator. Only call it
// once acquireTap has returned successfully (all four fields set).
func (r tapResources) release(ports *PortAllocator) {
	r.callerSide.close()
	r.calleeSide.close()
	ports.Release(r.callerPair)
	ports.Release(r.calleePair)
}

// errTapExhausted and errTapBind distinguish the two acquireTap failure modes so
// the caller can map them to the right inbound status (503 vs 500).
var (
	errTapExhausted = errors.New("tap port exhaustion")
	errTapBind      = errors.New("tap bind failed")
)

// acquireTap reserves two port pairs and binds both anchor sides for a tap.
// All-or-nothing: on any failure it releases whatever it already took and returns
// an error wrapping errTapExhausted (port acquire) or errTapBind (socket bind).
func (e *Engine) acquireTap() (tapResources, error) {
	callerPair, err := e.ports.Acquire()
	if err != nil {
		return tapResources{}, fmt.Errorf("%w: caller: %v", errTapExhausted, err)
	}
	calleePair, err := e.ports.Acquire()
	if err != nil {
		e.ports.Release(callerPair)
		return tapResources{}, fmt.Errorf("%w: callee: %v", errTapExhausted, err)
	}
	callerSide, err := newAnchorSide(e.mediaHost, callerPair)
	if err != nil {
		e.ports.Release(callerPair)
		e.ports.Release(calleePair)
		return tapResources{}, fmt.Errorf("%w: caller: %v", errTapBind, err)
	}
	calleeSide, err := newAnchorSide(e.mediaHost, calleePair)
	if err != nil {
		callerSide.close()
		e.ports.Release(callerPair)
		e.ports.Release(calleePair)
		return tapResources{}, fmt.Errorf("%w: callee: %v", errTapBind, err)
	}
	return tapResources{
		callerPair: callerPair,
		calleePair: calleePair,
		callerSide: callerSide,
		calleeSide: calleeSide,
	}, nil
}

// runAppChain originates one leg per configured application in sequence, applying
// each app's media mode and on-failure policy. It returns false when an app's
// failure policy aborts the call (final response already sent, call torn down).
func (e *Engine) runAppChain(ctx context.Context, call *Call) bool {
	for i := range e.cfg.Sequence {
		app := e.cfg.Sequence[i]

		var appURI sip.Uri
		if err := sip.ParseUri(app.URI, &appURI); err != nil {
			slog.Error("parse app URI", "uri", app.URI, "err", err)
			e.fail(call, 500, "Server Error", "bad app URI")
			return false
		}
		// Build the INVITE body for this app based on its media mode.
		hasTap := app.Media == config.MediaTap
		var inviteBody []byte
		var tap tapResources

		if hasTap {
			var err error
			tap, err = e.acquireTap()
			if err != nil {
				e.metrics.AppFailure(app.Name)
				slog.Warn("tap setup failed", "name", app.Name, "err", err)
				if failureAction(app.OnFailure) == actionAbort {
					status, reason := 500, "Server Error"
					if errors.Is(err, errTapExhausted) {
						status, reason = 503, "Service Unavailable"
					}
					e.fail(call, status, reason, fmt.Sprintf("tap setup for %q: %v", app.Name, err))
					return false
				}
				continue
			}
			inviteBody, err = buildTapOffer(call.inbound.offerSDP, e.mediaHost, tap.callerPair.RTP, tap.calleePair.RTP)
			if err != nil {
				tap.release(e.ports)
				slog.Error("build tap offer", "name", app.Name, "err", err)
				e.fail(call, 500, "Server Error", fmt.Sprintf("build tap offer for %q: %v", app.Name, err))
				return false
			}
		} else {
			var err error
			inviteBody, err = buildInactiveOffer(call.inbound.offerSDP, e.mediaHost)
			if err != nil {
				slog.Error("build inactive offer", "name", app.Name, "err", err)
				e.fail(call, 500, "Server Error", fmt.Sprintf("build inactive offer for %q: %v", app.Name, err))
				return false
			}
		}

		// Transport switch: a tls app dials over its profile's per-profile dialer with a
		// connect-timeout-bounded context; any other transport keeps the forced-TCP plain
		// dialer unchanged. The single transport value selects the path.
		cache := e.dialogCliCache
		dialCtx, dialCancel := ctx, context.CancelFunc(func() {})
		if app.Transport == config.TransportTLS {
			appURI = withTransport(appURI, "tls")
			cache = e.tlsDialers[app.TLSProfile]
			dialCtx, dialCancel = dialContext(ctx, app.Resolved)
		} else {
			appURI = withTCP(appURI)
		}

		appLegID := newLegID()
		appHeaders := append(call.forwardHeaders(),
			sip.NewHeader("X-Sequencer-Call-Id", call.id),
			sip.NewHeader("X-Sequencer-Leg-Id", appLegID))
		appSess, err := cache.Invite(dialCtx, appURI, inviteBody, appHeaders...)
		dialCancel()
		if err != nil {
			if hasTap {
				tap.release(e.ports)
			}
			e.metrics.AppFailure(app.Name)
			slog.Warn("application failed", "name", app.Name, "uri", app.URI, "policy", app.OnFailure, "stage", "originate", "err", err)
			if failureAction(app.OnFailure) == actionAbort {
				e.fail(call, 503, "Service Unavailable", fmt.Sprintf("app leg originate failed: %v", err))
				return false
			}
			continue
		}

		legCtx, legCancel := context.WithTimeout(ctx, e.legTimeout)
		appErr := appSess.WaitAnswer(legCtx, sipgo.AnswerOptions{
			OnResponse: func(res *sip.Response) error {
				if res.IsProvisional() && res.StatusCode != 100 {
					_ = call.inbound.session.Respond(res.StatusCode, res.Reason, res.Body(),
						relayableResponseHeaders(res, res.Body())...)
				}
				return nil
			},
		})
		legCancel()

		if appErr != nil {
			if hasTap {
				tap.release(e.ports)
			}
			e.metrics.AppFailure(app.Name)
			slog.Warn("application failed", "name", app.Name, "uri", app.URI, "policy", app.OnFailure, "stage", "answer", "err", appErr)
			if failureAction(app.OnFailure) == actionAbort {
				var dialErr *sipgo.ErrDialogResponse
				if errors.As(appErr, &dialErr) {
					status := mapFailureStatus(failureReject, dialErr.Res.StatusCode)
					_ = call.inbound.session.Respond(status, dialErr.Res.Reason, dialErr.Res.Body(),
						relayableResponseHeaders(dialErr.Res, dialErr.Res.Body())...)
				} else {
					_ = call.inbound.session.Respond(mapFailureStatus(failureTimeout, 0), "Service Unavailable", nil)
				}
				releasePendingTaps(call, e.ports)
				call.teardown(fmt.Sprintf("app leg %q failed: %v", app.URI, appErr))
				return false
			}
			continue
		}

		appResp := appSess.InviteResponse
		if err := appSess.Ack(ctx); err != nil {
			slog.Warn("ACK app leg", "uri", app.URI, "err", err)
		}

		// Stash tap if the app accepted a dual-stream recvonly offer.
		if hasTap {
			h1, p1, h2, p2, parseErr := parseTapAnswer(copyBody(appResp.Body()))
			if parseErr != nil {
				slog.Warn("parse tap answer", "name", app.Name, "err", parseErr)
				// Treat parse failure same as app failure.
				tap.release(e.ports)
			} else {
				if p1 > 0 {
					tap.callerSide.setRemote(&net.UDPAddr{IP: net.ParseIP(h1), Port: p1}, nil)
				}
				if p2 > 0 {
					tap.calleeSide.setRemote(&net.UDPAddr{IP: net.ParseIP(h2), Port: p2}, nil)
				}
				t := &Tap{
					appName:      app.Name,
					callerStream: tap.callerSide,
					calleeStream: tap.calleeSide,
					callerPair:   tap.callerPair,
					calleePair:   tap.calleePair,
				}
				call.mu.Lock()
				call.pendingTaps = append(call.pendingTaps, pendingTap{
					tap:        t,
					callerPair: tap.callerPair,
					calleePair: tap.calleePair,
				})
				call.mu.Unlock()
			}
		}

		call.mu.Lock()
		call.appLegs = append(call.appLegs, &OutboundLeg{
			role:      roleApplication,
			targetURI: app.URI,
			legID:     appLegID,
			session:   appSess,
			answerSDP: copyBody(appResp.Body()),
		})
		// App answers no longer feed the call SDP — call anchor stays inbound-offer→PBX.
		call.mu.Unlock()

		e.metrics.AppInvocation(app.Name)

		appSess.OnState(func(s sip.DialogState) {
			if s == sip.DialogStateEnded {
				call.teardown("app dialog ended")
			}
		})
	}
	return true
}

// mediaAnchor carries the anchored media state the PBX phase needs after the
// endpoint-facing anchor is established.
type mediaAnchor struct {
	session *MediaSession
	pbxSide *AnchorSide
	epPair  PortPair
	pbxPair PortPair
}

// anchorMedia parses the inbound offer, acquires and binds the endpoint- and
// PBX-facing port pairs, wires the endpoint remote, and registers the media
// session (with its teardown release hook) on the call. It returns false when a
// failure aborts the call (final response already sent, call torn down).
func (e *Engine) anchorMedia(call *Call) (mediaAnchor, bool) {
	epHost, epPort, err := parseMedia(call.inbound.offerSDP)
	if err != nil {
		slog.Error("parse inbound SDP", "err", err)
		e.fail(call, 488, "Not Acceptable Here", "bad inbound SDP")
		return mediaAnchor{}, false
	}

	epPair, err := e.ports.Acquire()
	if err != nil {
		slog.Error("acquire endpoint media ports", "err", err)
		e.fail(call, 503, "Service Unavailable", "port exhaustion")
		return mediaAnchor{}, false
	}
	pbxPair, err := e.ports.Acquire()
	if err != nil {
		e.ports.Release(epPair)
		slog.Error("acquire pbx media ports", "err", err)
		e.fail(call, 503, "Service Unavailable", "port exhaustion")
		return mediaAnchor{}, false
	}

	epSide, err := newAnchorSide(e.mediaHost, epPair)
	if err != nil {
		e.ports.Release(epPair)
		e.ports.Release(pbxPair)
		slog.Error("bind endpoint media sockets", "err", err)
		e.fail(call, 500, "Server Error", "media bind failed")
		return mediaAnchor{}, false
	}
	pbxSide, err := newAnchorSide(e.mediaHost, pbxPair)
	if err != nil {
		epSide.close()
		e.ports.Release(epPair)
		e.ports.Release(pbxPair)
		slog.Error("bind pbx media sockets", "err", err)
		e.fail(call, 500, "Server Error", "media bind failed")
		return mediaAnchor{}, false
	}

	epSide.setRemote(
		&net.UDPAddr{IP: net.ParseIP(epHost), Port: epPort},
		&net.UDPAddr{IP: net.ParseIP(epHost), Port: epPort + 1},
	)

	// Register release hook so teardown cleans up even on mid-setup failure.
	// Tap pairs are released here too (they are on mediaSess.taps after registration).
	mediaSess := &MediaSession{endpointSide: epSide, pbxSide: pbxSide}
	ports := e.ports
	call.mu.Lock()
	call.media = mediaSess
	call.releaseMedia = func() {
		mediaSess.Close()
		ports.Release(epPair)
		ports.Release(pbxPair)
		for _, t := range mediaSess.tapList() {
			ports.Release(t.callerPair)
			ports.Release(t.calleePair)
		}
	}
	call.mu.Unlock()

	return mediaAnchor{session: mediaSess, pbxSide: pbxSide, epPair: epPair, pbxPair: pbxPair}, true
}

// dialPBX originates the terminating PBX leg, anchors the PBX-side media from its
// answer, answers the inbound caller with the anchored SDP, marks the call
// established, and registers the stashed taps. It returns false when a failure
// aborts the call (final response already sent, call torn down).
func (e *Engine) dialPBX(ctx context.Context, call *Call, anchor mediaAnchor, start time.Time) bool {
	pbxOffer, err := rewriteToAnchor(call.inbound.offerSDP, e.mediaHost, anchor.pbxPair.RTP)
	if err != nil {
		slog.Error("rewrite SDP for PBX", "err", err)
		e.fail(call, 500, "Server Error", "rewrite sdp: "+err.Error())
		return false
	}

	var pbxURI sip.Uri
	if err := sip.ParseUri(e.cfg.NextHop.URI, &pbxURI); err != nil {
		slog.Error("parse pbx URI", "uri", e.cfg.NextHop.URI, "err", err)
		e.fail(call, 500, "Server Error", "bad pbx URI")
		return false
	}

	// Next-hop transport switch: tls dials over its profile's per-profile dialer with a
	// connect-timeout-bounded context; tcp forces transport=tcp on the plain dialer;
	// udp/unset keeps today's plain UDP path unchanged.
	cache := e.dialogCliCache
	dialCtx, dialCancel := ctx, context.CancelFunc(func() {})
	switch e.cfg.NextHop.Transport {
	case config.TransportTLS:
		pbxURI = withTransport(pbxURI, "tls")
		cache = e.tlsDialers[e.cfg.NextHop.TLSProfile]
		dialCtx, dialCancel = dialContext(ctx, e.cfg.NextHop.Resolved)
	case config.TransportTCP:
		pbxURI = withTransport(pbxURI, "tcp")
	}

	pbxLegID := newLegID()
	pbxHeaders := append(call.forwardHeaders(),
		sip.NewHeader("X-Sequencer-Call-Id", call.id),
		sip.NewHeader("X-Sequencer-Leg-Id", pbxLegID))
	pbxSess, err := cache.Invite(dialCtx, pbxURI, pbxOffer, pbxHeaders...)
	dialCancel()
	if err != nil {
		e.metrics.TerminatingHopFailure()
		slog.Error("originate pbx leg", "uri", e.cfg.NextHop.URI, "err", err)
		e.fail(call, 503, "Service Unavailable", "pbx leg originate failed")
		return false
	}
	call.mu.Lock()
	call.pbxLeg = &OutboundLeg{
		role:      rolePBX,
		targetURI: e.cfg.NextHop.URI,
		legID:     pbxLegID,
		session:   pbxSess,
	}
	call.mu.Unlock()

	legCtx, legCancel := context.WithTimeout(ctx, e.legTimeout)
	pbxErr := pbxSess.WaitAnswer(legCtx, sipgo.AnswerOptions{
		OnResponse: func(res *sip.Response) error {
			if res.IsProvisional() && res.StatusCode != 100 {
				_ = call.inbound.session.Respond(res.StatusCode, res.Reason, res.Body(),
					relayableResponseHeaders(res, res.Body())...)
			}
			return nil
		},
	})
	legCancel()

	if pbxErr != nil {
		e.metrics.TerminatingHopFailure()
		var dialErr *sipgo.ErrDialogResponse
		if errors.As(pbxErr, &dialErr) {
			status := mapFailureStatus(failureReject, dialErr.Res.StatusCode)
			_ = call.inbound.session.Respond(status, dialErr.Res.Reason, dialErr.Res.Body(),
				relayableResponseHeaders(dialErr.Res, dialErr.Res.Body())...)
		} else {
			_ = call.inbound.session.Respond(mapFailureStatus(failureTimeout, 0), "Service Unavailable", nil)
		}
		releasePendingTaps(call, e.ports)
		call.teardown(fmt.Sprintf("pbx leg failed: %v", pbxErr))
		return false
	}

	pbxResp := pbxSess.InviteResponse
	if err := pbxSess.Ack(ctx); err != nil {
		slog.Warn("ACK pbx leg", "err", err)
	}

	pbxAnswerRaw := copyBody(pbxResp.Body())
	call.mu.Lock()
	call.pbxLeg.answerSDP = pbxAnswerRaw
	call.mu.Unlock()

	pbxSess.OnState(func(s sip.DialogState) {
		if s == sip.DialogStateEnded {
			call.teardown("pbx dialog ended")
		}
	})

	// Wire PBX-side remote address from PBX answer.
	pbxHost, pbxRTPPort, err := parseMedia(pbxAnswerRaw)
	if err != nil {
		e.metrics.TerminatingHopFailure()
		slog.Error("parse pbx answer SDP", "err", err)
		e.fail(call, 488, "Not Acceptable Here", "bad pbx answer SDP")
		return false
	}
	anchor.pbxSide.setRemote(
		&net.UDPAddr{IP: net.ParseIP(pbxHost), Port: pbxRTPPort},
		&net.UDPAddr{IP: net.ParseIP(pbxHost), Port: pbxRTPPort + 1},
	)

	// Rewrite PBX answer for the endpoint: codec from PBX, address from sequencer.
	epAnswerSDP, err := rewriteToAnchor(pbxAnswerRaw, e.mediaHost, anchor.epPair.RTP)
	if err != nil {
		slog.Error("rewrite SDP for endpoint", "err", err)
		e.fail(call, 500, "Server Error", "rewrite sdp: "+err.Error())
		return false
	}

	// answer the endpoint with the anchored SDP, relaying the PBX 200's headers
	if err := call.inbound.session.Respond(200, "OK", epAnswerSDP,
		relayableResponseHeaders(pbxResp, epAnswerSDP)...); err != nil {
		slog.Error("answer endpoint", "err", err)
		releasePendingTaps(call, e.ports)
		call.teardown("answer endpoint failed")
		return false
	}

	e.metrics.ObserveSequencingLatency(time.Since(start))

	call.mu.Lock()
	if canTransition(call.state, stateEstablished) {
		call.state = stateEstablished
	}
	// Register all pending taps on the media session before relay starts.
	for _, pt := range call.pendingTaps {
		anchor.session.addTap(pt.tap)
	}
	call.pendingTaps = nil
	call.mu.Unlock()

	return true
}

// releasePendingTaps releases port pairs for taps stashed on the call that were never
// registered on a MediaSession (call failed before PBX 2xx).
func releasePendingTaps(call *Call, ports *PortAllocator) {
	call.mu.Lock()
	pts := call.pendingTaps
	call.pendingTaps = nil
	call.mu.Unlock()
	for _, pt := range pts {
		pt.tap.close()
		ports.Release(pt.callerPair)
		ports.Release(pt.calleePair)
	}
}

func copyBody(b []byte) []byte {
	if b == nil {
		return nil
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp
}
