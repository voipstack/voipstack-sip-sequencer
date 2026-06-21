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

// dialerFor selects the dialog client cache and the transport-forced URI for an
// outbound leg, plus a dial context bounded by the profile's connect timeout. A tls leg
// dials over its per-profile dialer with the bounded context; any other transport uses
// the plain dialer and, when plainTransport is non-empty, forces it onto the URI
// ("tcp" for app legs, which must run over TCP; "tcp"/"" for the next hop's tcp/udp
// default). The caller must invoke the returned cancel once the dial returns.
func (e *Engine) dialerFor(ctx context.Context, uri sip.Uri, transport config.Transport, tlsProfile string, resolved *config.ResolvedTLSProfile, plainTransport string) (*sipgo.DialogClientCache, sip.Uri, context.Context, context.CancelFunc) {
	if transport == config.TransportTLS {
		dialCtx, cancel := dialContext(ctx, resolved)
		return e.tlsDialers[tlsProfile], withTransport(uri, "tls"), dialCtx, cancel
	}
	if plainTransport != "" {
		uri = withTransport(uri, plainTransport)
	}
	return e.dialogCliCache, uri, ctx, func() {}
}

// relayProvisional builds AnswerOptions whose OnResponse forwards each non-100
// provisional from an outbound leg to the inbound caller, preserving relayable headers.
func (c *Call) relayProvisional() sipgo.AnswerOptions {
	return sipgo.AnswerOptions{
		OnResponse: func(res *sip.Response) error {
			if res.IsProvisional() && res.StatusCode != 100 {
				_ = c.inbound.session.Respond(res.StatusCode, res.Reason, res.Body(),
					relayableResponseHeaders(res, res.Body())...)
			}
			return nil
		},
	}
}

// respondInboundFromLegError sends the inbound caller a final response derived from a
// failed outbound leg — relaying the leg's own status/reason/body when it rejected with
// a SIP response, or a timeout 503 otherwise — then releases pending taps and tears the
// call down with cause.
func (e *Engine) respondInboundFromLegError(call *Call, legErr error, cause string) {
	var dialErr *sipgo.ErrDialogResponse
	if errors.As(legErr, &dialErr) {
		status := mapFailureStatus(failureReject, dialErr.Res.StatusCode)
		_ = call.inbound.session.Respond(status, dialErr.Res.Reason, dialErr.Res.Body(),
			relayableResponseHeaders(dialErr.Res, dialErr.Res.Body())...)
	} else {
		_ = call.inbound.session.Respond(mapFailureStatus(failureTimeout, 0), "Service Unavailable", nil)
	}
	releasePendingTaps(call, e.ports)
	call.teardown(cause)
}

// mediaReleaser builds the call's releaseMedia hook: it closes the media session
// (sockets and any secured leg) and returns every port pair — the fixed ep/pbx pairs
// passed in plus each registered tap's pair — to the allocator. Centralizing the tap
// loop keeps the three setup paths from each re-deriving (and risking dropping) it.
func mediaReleaser(sess *MediaSession, ports *PortAllocator, fixed ...PortPair) func() {
	return func() {
		sess.Close()
		for _, p := range fixed {
			ports.Release(p)
		}
		for _, t := range sess.tapList() {
			ports.Release(t.callerPair)
			ports.Release(t.calleePair)
		}
	}
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
			// After a REFER transfer the inbound leg is detached and BYE'd on purpose;
			// its end must not collapse the transferred call.
			if call.inboundTeardownSuppressed() {
				return
			}
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

	// A webphone (WebRTC) endpoint: dial the plain PBX, answer the webphone with its
	// DTLS-SRTP SDP, and bridge the secured leg to the plain PBX leg (STORY-001-021).
	if anchor.securedLeg != nil {
		if !e.bridgeSecuredToPBX(ctx, call, anchor, start) {
			return
		}
		<-ctx.Done()
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

// appLegFailed performs the bookkeeping shared by every app-leg failure (originate or
// answer): it releases a pending tap, emits the failure metric, and logs the failure with
// its stage. It returns true when the app's policy requires aborting the call — the caller
// then sends the stage-specific final response and returns — and false to skip to the next
// application.
func (e *Engine) appLegFailed(app config.Application, stage string, hasTap bool, tap tapResources, err error) bool {
	if hasTap {
		tap.release(e.ports)
	}
	e.metrics.AppFailure(app.Name)
	slog.Warn("application failed", "name", app.Name, "uri", app.URI, "policy", app.OnFailure, "stage", stage, "err", err)
	return failureAction(app.OnFailure) == actionAbort
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

		// A single deadline bounds this app's whole setup span (dial + answer wait). The
		// dial bound is what fast-fails an unreachable/silent plain-TCP app — sipgo can only
		// CANCEL after a provisional (RFC 3261 §9.1), so the answer-wait portion alone cannot
		// cut a silent peer short of Timer B. A per-app timeout overrides the global default.
		appCtx, appCancel := context.WithTimeout(ctx, effectiveTimeout(app, e.legTimeout))

		// A plain application leg always runs over TCP (the tap-mode offer can exceed
		// sipgo's UDP MTU guard); a tls app dials over its profile's per-profile dialer.
		cache, appURI, dialCtx, dialCancel := e.dialerFor(appCtx, appURI, app.Transport, app.TLSProfile, app.Resolved, "tcp")

		appLegID := newLegID()
		appHeaders := append(call.forwardHeaders(),
			sip.NewHeader("X-Sequencer-Call-Id", call.id),
			sip.NewHeader("X-Sequencer-Leg-Id", appLegID))
		appSess, err := cache.Invite(dialCtx, appURI, inviteBody, appHeaders...)
		dialCancel()
		if err != nil {
			appCancel()
			if e.appLegFailed(app, "originate", hasTap, tap, err) {
				e.fail(call, 503, "Service Unavailable", fmt.Sprintf("app leg originate failed: %v", err))
				return false
			}
			continue
		}

		appErr := appSess.WaitAnswer(appCtx, call.relayProvisional())
		appCancel()

		if appErr != nil {
			if e.appLegFailed(app, "answer", hasTap, tap, appErr) {
				e.respondInboundFromLegError(call, appErr, fmt.Sprintf("app leg %q failed: %v", app.URI, appErr))
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
// endpoint-facing anchor is established. securedLeg is set only when the endpoint is
// a WebRTC (DTLS-SRTP) leg, in which case the plain pbxSide/epPair fields are unused.
type mediaAnchor struct {
	session    *MediaSession
	pbxSide    *AnchorSide
	epPair     PortPair
	pbxPair    PortPair
	securedLeg *SecuredLeg
}

// anchorMedia parses the inbound offer, acquires and binds the endpoint- and
// PBX-facing port pairs, wires the endpoint remote, and registers the media
// session (with its teardown release hook) on the call. It returns false when a
// failure aborts the call (final response already sent, call torn down).
func (e *Engine) anchorMedia(call *Call) (mediaAnchor, bool) {
	// A webphone offers WebRTC media; its endpoint leg is a secured (DTLS-SRTP)
	// endpoint rather than a plain anchor. The plain path below is untouched.
	if offerIsWebRTC(call.inbound.offerSDP) {
		return e.anchorWebRTC(call)
	}

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
	call.mu.Lock()
	call.media = mediaSess
	call.releaseMedia = mediaReleaser(mediaSess, e.ports, epPair, pbxPair)
	call.mu.Unlock()

	return mediaAnchor{session: mediaSess, pbxSide: pbxSide, epPair: epPair, pbxPair: pbxPair}, true
}

// anchorWebRTC brings up the secured (DTLS-SRTP) endpoint leg for a webphone's WebRTC
// offer: it builds the leg via the WebRTC factory (which answers ICE-lite, gathers a
// host candidate on the configured public address, and terminates DTLS-SRTP), and
// registers it on a MediaSession so teardown unwinds the pion endpoint. It binds no
// RTP port pair (pion owns its own port) and no PBX side — dialing the PBX and
// bridging is STORY-001-021. It returns false when setup fails (final response sent,
// call torn down).
func (e *Engine) anchorWebRTC(call *Call) (mediaAnchor, bool) {
	leg, err := newSecuredLeg(e.webrtcFactory, e.mediaPublicAddr, call.inbound.offerSDP)
	if err != nil {
		slog.Error("bring up webrtc endpoint leg", "err", err)
		e.fail(call, 488, "Not Acceptable Here", "webrtc endpoint setup failed")
		return mediaAnchor{}, false
	}

	mediaSess := &MediaSession{endpointLeg: leg}
	call.mu.Lock()
	call.media = mediaSess
	call.releaseMedia = mediaReleaser(mediaSess, e.ports)
	call.mu.Unlock()

	return mediaAnchor{session: mediaSess, securedLeg: leg}, true
}

// answerSecuredEndpoint answers the webphone with the secured leg's ICE-lite/DTLS-SRTP
// SDP, marks the call established, and registers the stashed taps. It returns false
// when answering fails (call torn down).
func (e *Engine) answerSecuredEndpoint(call *Call, anchor mediaAnchor, start time.Time) bool {
	// Mark established and register taps before sending 200 OK, so an immediate in-dialog
	// re-INVITE/REFER from the webphone is handled (not 481'd) the instant it learns the
	// call is up.
	call.mu.Lock()
	if canTransition(call.state, stateEstablished) {
		call.state = stateEstablished
	}
	for _, pt := range call.pendingTaps {
		anchor.session.addTap(pt.tap)
	}
	call.pendingTaps = nil
	call.mu.Unlock()

	answerSDP := anchor.securedLeg.AnswerSDP()
	if err := call.inbound.session.Respond(200, "OK", answerSDP); err != nil {
		slog.Error("answer webphone endpoint", "err", err)
		releasePendingTaps(call, e.ports)
		call.teardown("answer webphone endpoint failed")
		return false
	}

	e.metrics.ObserveSequencingLatency(time.Since(start))

	return true
}

// bridgeSecuredToPBX completes the secured (webphone) path: it binds the plain
// PBX-facing anchor side (pion owns the webphone side's port), dials the PBX with a
// plain RTP/AVP offer carrying the negotiated codecs (no transcoding), anchors the PBX
// side to the PBX answer, answers the webphone with its DTLS-SRTP SDP, and starts the
// security-agnostic media bridge between the secured endpoint leg and the plain PBX leg.
// It returns false when a failure aborts the call (final response already sent, call
// torn down).
func (e *Engine) bridgeSecuredToPBX(ctx context.Context, call *Call, anchor mediaAnchor, start time.Time) bool {
	pbxPair, err := e.ports.Acquire()
	if err != nil {
		e.fail(call, 503, "Service Unavailable", "rtp ports exhausted")
		return false
	}
	pbxSide, err := newAnchorSide(e.mediaHost, pbxPair)
	if err != nil {
		e.ports.Release(pbxPair)
		slog.Error("bind pbx media sockets", "err", err)
		e.fail(call, 500, "Server Error", "media bind failed")
		return false
	}

	// Register the PBX side on the existing media session (the secured endpoint leg is
	// already set) and extend the release hook to free the PBX pair on teardown.
	sess := anchor.session
	call.mu.Lock()
	sess.pbxSide = pbxSide
	call.releaseMedia = mediaReleaser(sess, e.ports, pbxPair)
	call.mu.Unlock()

	// Plain RTP/AVP offer toward the PBX: same codecs, no ICE/DTLS/rtcp-mux.
	pbxOffer, err := buildPlainOfferFromWebRTC(call.inbound.offerSDP, e.mediaHost, pbxPair.RTP)
	if err != nil {
		slog.Error("build pbx offer", "err", err)
		e.fail(call, 500, "Server Error", "build pbx offer: "+err.Error())
		return false
	}

	_, pbxAnswerRaw, ok := e.originatePBX(ctx, call, pbxOffer)
	if !ok {
		return false
	}

	pbxHost, pbxRTPPort, err := parseMedia(pbxAnswerRaw)
	if err != nil {
		e.metrics.TerminatingHopFailure()
		slog.Error("parse pbx answer SDP", "err", err)
		e.fail(call, 488, "Not Acceptable Here", "bad pbx answer SDP")
		return false
	}
	pbxSide.setRemote(
		&net.UDPAddr{IP: net.ParseIP(pbxHost), Port: pbxRTPPort},
		&net.UDPAddr{IP: net.ParseIP(pbxHost), Port: pbxRTPPort + 1},
	)

	// Both legs are now negotiated: the WebRTC endpoint answered one codec and the PBX
	// another may differ. Surface a mismatch (logging/metric only) — the secured↔plain
	// bridge forwards RTP byte-for-byte and never transcodes.
	e.reportCodecAgreement(call, anchor.securedLeg.AnswerSDP(), pbxAnswerRaw)

	// Answer the webphone with the secured leg's ICE-lite/DTLS-SRTP SDP (registers taps,
	// transitions the call to established).
	if !e.answerSecuredEndpoint(call, anchor, start) {
		return false
	}

	// Bridge the secured endpoint leg to the plain PBX leg, fanning RTP out to taps.
	taps := sess.tapList()
	callerTaps := make([]*AnchorSide, 0, len(taps))
	calleeTaps := make([]*AnchorSide, 0, len(taps))
	for _, t := range taps {
		callerTaps = append(callerTaps, t.callerStream)
		calleeTaps = append(calleeTaps, t.calleeStream)
	}
	go sess.bridgeLegs(ctx, anchor.securedLeg, pbxSide, callerTaps, calleeTaps)

	return true
}

// originatePBX dials the terminating PBX leg with the given offer, relaying provisional
// responses to the inbound caller, waits for the answer, ACKs it, records the PBX leg
// and its teardown hook, and returns the PBX 2xx response and its raw answer SDP. It is
// the offer-agnostic core shared by the plain path (dialPBX) and the secured-bridge path
// (bridgeSecuredToPBX). It returns ok=false when a failure aborts the call (final
// response already sent, call torn down).
func (e *Engine) originatePBX(ctx context.Context, call *Call, pbxOffer []byte) (*sip.Response, []byte, bool) {
	var pbxURI sip.Uri
	if err := sip.ParseUri(e.cfg.NextHop.URI, &pbxURI); err != nil {
		slog.Error("parse pbx URI", "uri", e.cfg.NextHop.URI, "err", err)
		e.fail(call, 500, "Server Error", "bad pbx URI")
		return nil, nil, false
	}

	// The next hop honors its configured transport: tls over the per-profile dialer, tcp
	// forced onto the URI, udp/unset left as plain UDP.
	plainTransport := ""
	if e.cfg.NextHop.Transport == config.TransportTCP {
		plainTransport = "tcp"
	}
	cache, pbxURI, dialCtx, dialCancel := e.dialerFor(ctx, pbxURI, e.cfg.NextHop.Transport, e.cfg.NextHop.TLSProfile, e.cfg.NextHop.Resolved, plainTransport)

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
		return nil, nil, false
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
	pbxErr := pbxSess.WaitAnswer(legCtx, call.relayProvisional())
	legCancel()

	if pbxErr != nil {
		e.metrics.TerminatingHopFailure()
		e.respondInboundFromLegError(call, pbxErr, fmt.Sprintf("pbx leg failed: %v", pbxErr))
		return nil, nil, false
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

	return pbxResp, pbxAnswerRaw, true
}

// reportCodecAgreement compares the agreed audio codec on the two anchored legs and,
// when they differ, emits one ERROR log and increments the mismatch metric: the
// sequencer never transcodes, so legs that disagree relay undecodable RTP and the call
// is connected but silent. Matching legs log at debug. This is logging only — it never
// changes the call flow; an SDP that does not parse logs at debug and is treated as
// "cannot tell", not as a mismatch. endpointSDP and pbxSDP are each leg's final
// negotiated SDP (the endpoint answer and the PBX answer).
func (e *Engine) reportCodecAgreement(call *Call, endpointSDP, pbxSDP []byte) {
	ep, err := selectedAudioCodec(endpointSDP)
	if err != nil {
		slog.Debug("codec check: parse endpoint answer SDP", "call", call.id, "err", err)
		return
	}
	pbx, err := selectedAudioCodec(pbxSDP)
	if err != nil {
		slog.Debug("codec check: parse pbx answer SDP", "call", call.id, "err", err)
		return
	}

	pbxLegID := ""
	call.mu.Lock()
	if call.pbxLeg != nil {
		pbxLegID = call.pbxLeg.legID
	}
	call.mu.Unlock()

	if codecsMatch(ep, pbx) {
		slog.Debug("anchored legs agree on audio codec",
			"call", call.id, "codec", ep.Label(), "clock", ep.ClockRate)
		return
	}

	slog.Error("media codec mismatch: call will have no usable audio (sequencer does not transcode)",
		"call", call.id, "pbx_leg", pbxLegID,
		"endpoint_codec", ep.Label(), "endpoint_pt", ep.PayloadType,
		"pbx_codec", pbx.Label(), "pbx_pt", pbx.PayloadType)
	e.metrics.MediaCodecMismatch(ep.Label(), pbx.Label())
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

	pbxResp, pbxAnswerRaw, ok := e.originatePBX(ctx, call, pbxOffer)
	if !ok {
		return false
	}

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

	// Both legs are now negotiated: surface a codec mismatch (logging/metric only).
	e.reportCodecAgreement(call, epAnswerSDP, pbxAnswerRaw)

	// Mark established and register taps BEFORE sending 200 OK, so an immediate
	// in-dialog re-INVITE/REFER from the endpoint is handled (not 403/481'd) the
	// instant it learns the call is up. Mirrors answerSecuredEndpoint; doing this after
	// the 200 leaves a window where the caller's mid-dialog request races the
	// transition and is rejected.
	call.mu.Lock()
	if canTransition(call.state, stateEstablished) {
		call.state = stateEstablished
	}
	for _, pt := range call.pendingTaps {
		anchor.session.addTap(pt.tap)
	}
	call.pendingTaps = nil
	call.mu.Unlock()

	// answer the endpoint with the anchored SDP, relaying the PBX 200's headers
	if err := call.inbound.session.Respond(200, "OK", epAnswerSDP,
		relayableResponseHeaders(pbxResp, epAnswerSDP)...); err != nil {
		slog.Error("answer endpoint", "err", err)
		releasePendingTaps(call, e.ports)
		call.teardown("answer endpoint failed")
		return false
	}

	e.metrics.ObserveSequencingLatency(time.Since(start))

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
