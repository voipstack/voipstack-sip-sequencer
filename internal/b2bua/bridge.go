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

// withTCP returns u with its transport URI parameter forced to tcp, overriding any
// existing value. It does not mutate u's params (the clone is fresh).
func withTCP(u sip.Uri) sip.Uri {
	params := u.UriParams.Clone()
	params.Add("transport", "tcp")
	u.UriParams = params
	return u
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

// bridge sequences the app chain and PBX leg for one call. All sipgo I/O lives here;
// pure helpers (mapFailureStatus, canTransition) live in state.go.
func (e *Engine) bridge(ctx context.Context, call *Call) {
	start := time.Now()
	// ── app chain ────────────────────────────────────────────────────────────
	for i := range e.cfg.Sequence {
		app := e.cfg.Sequence[i]

		var appURI sip.Uri
		if err := sip.ParseUri(app.URI, &appURI); err != nil {
			slog.Error("parse app URI", "uri", app.URI, "err", err)
			_ = call.inbound.session.Respond(500, "Server Error", nil)
			releasePendingTaps(call, e.ports)
			call.teardown("bad app URI")
			return
		}
		// Application legs always run over TCP: the offer carries the caller's SDP
		// (grown further in tap mode), which can exceed sipgo's UDP MTU guard.
		appURI = withTCP(appURI)

		// Build the INVITE body for this app based on its media mode.
		var inviteBody []byte
		var tapCallerPair, tapCalleePair PortPair
		var tapCallerSide, tapCalleeSide *AnchorSide

		if app.Media == config.MediaTap {
			var err error
			tapCallerPair, err = e.ports.Acquire()
			if err != nil {
				e.metrics.AppFailure(app.Name)
				slog.Warn("tap port exhaustion", "name", app.Name, "err", err)
				if failureAction(app.OnFailure) == actionAbort {
					_ = call.inbound.session.Respond(503, "Service Unavailable", nil)
					releasePendingTaps(call, e.ports)
					call.teardown(fmt.Sprintf("tap port exhaustion for %q: %v", app.Name, err))
					return
				}
				continue
			}
			tapCalleePair, err = e.ports.Acquire()
			if err != nil {
				e.ports.Release(tapCallerPair)
				e.metrics.AppFailure(app.Name)
				slog.Warn("tap port exhaustion", "name", app.Name, "err", err)
				if failureAction(app.OnFailure) == actionAbort {
					_ = call.inbound.session.Respond(503, "Service Unavailable", nil)
					releasePendingTaps(call, e.ports)
					call.teardown(fmt.Sprintf("tap port exhaustion for %q: %v", app.Name, err))
					return
				}
				continue
			}
			tapCallerSide, err = newAnchorSide(e.mediaHost, tapCallerPair)
			if err != nil {
				e.ports.Release(tapCallerPair)
				e.ports.Release(tapCalleePair)
				e.metrics.AppFailure(app.Name)
				slog.Warn("tap bind failed", "name", app.Name, "err", err)
				if failureAction(app.OnFailure) == actionAbort {
					_ = call.inbound.session.Respond(500, "Server Error", nil)
					releasePendingTaps(call, e.ports)
					call.teardown(fmt.Sprintf("tap bind failed for %q: %v", app.Name, err))
					return
				}
				continue
			}
			tapCalleeSide, err = newAnchorSide(e.mediaHost, tapCalleePair)
			if err != nil {
				tapCallerSide.close()
				e.ports.Release(tapCallerPair)
				e.ports.Release(tapCalleePair)
				e.metrics.AppFailure(app.Name)
				slog.Warn("tap bind failed", "name", app.Name, "err", err)
				if failureAction(app.OnFailure) == actionAbort {
					_ = call.inbound.session.Respond(500, "Server Error", nil)
					releasePendingTaps(call, e.ports)
					call.teardown(fmt.Sprintf("tap bind failed for %q: %v", app.Name, err))
					return
				}
				continue
			}
			inviteBody, err = buildTapOffer(call.inbound.offerSDP, e.mediaHost, tapCallerPair.RTP, tapCalleePair.RTP)
			if err != nil {
				tapCallerSide.close()
				tapCalleeSide.close()
				e.ports.Release(tapCallerPair)
				e.ports.Release(tapCalleePair)
				slog.Error("build tap offer", "name", app.Name, "err", err)
				_ = call.inbound.session.Respond(500, "Server Error", nil)
				releasePendingTaps(call, e.ports)
				call.teardown(fmt.Sprintf("build tap offer for %q: %v", app.Name, err))
				return
			}
		} else {
			var err error
			inviteBody, err = buildInactiveOffer(call.inbound.offerSDP, e.mediaHost)
			if err != nil {
				slog.Error("build inactive offer", "name", app.Name, "err", err)
				_ = call.inbound.session.Respond(500, "Server Error", nil)
				releasePendingTaps(call, e.ports)
				call.teardown(fmt.Sprintf("build inactive offer for %q: %v", app.Name, err))
				return
			}
		}

		appLegID := newLegID()
		appHeaders := append(call.forwardHeaders(),
			sip.NewHeader("X-Sequencer-Call-Id", call.id),
			sip.NewHeader("X-Sequencer-Leg-Id", appLegID))
		appSess, err := e.dialogCliCache.Invite(ctx, appURI, inviteBody, appHeaders...)
		if err != nil {
			if app.Media == config.MediaTap {
				tapCallerSide.close()
				tapCalleeSide.close()
				e.ports.Release(tapCallerPair)
				e.ports.Release(tapCalleePair)
			}
			e.metrics.AppFailure(app.Name)
			slog.Warn("application failed", "name", app.Name, "uri", app.URI, "policy", app.OnFailure, "stage", "originate", "err", err)
			if failureAction(app.OnFailure) == actionAbort {
				_ = call.inbound.session.Respond(503, "Service Unavailable", nil)
				releasePendingTaps(call, e.ports)
				call.teardown(fmt.Sprintf("app leg originate failed: %v", err))
				return
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
			if app.Media == config.MediaTap {
				tapCallerSide.close()
				tapCalleeSide.close()
				e.ports.Release(tapCallerPair)
				e.ports.Release(tapCalleePair)
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
				return
			}
			continue
		}

		appResp := appSess.InviteResponse
		if err := appSess.Ack(ctx); err != nil {
			slog.Warn("ACK app leg", "uri", app.URI, "err", err)
		}

		// Stash tap if the app accepted a dual-stream recvonly offer.
		if app.Media == config.MediaTap {
			h1, p1, h2, p2, parseErr := parseTapAnswer(copyBody(appResp.Body()))
			if parseErr != nil {
				slog.Warn("parse tap answer", "name", app.Name, "err", parseErr)
				// Treat parse failure same as app failure.
				tapCallerSide.close()
				tapCalleeSide.close()
				e.ports.Release(tapCallerPair)
				e.ports.Release(tapCalleePair)
			} else {
				if p1 > 0 {
					tapCallerSide.setRemote(&net.UDPAddr{IP: net.ParseIP(h1), Port: p1}, nil)
				}
				if p2 > 0 {
					tapCalleeSide.setRemote(&net.UDPAddr{IP: net.ParseIP(h2), Port: p2}, nil)
				}
				tap := &Tap{
					appName:      app.Name,
					callerStream: tapCallerSide,
					calleeStream: tapCalleeSide,
					callerPair:   tapCallerPair,
					calleePair:   tapCalleePair,
				}
				call.mu.Lock()
				call.pendingTaps = append(call.pendingTaps, pendingTap{
					tap:        tap,
					callerPair: tapCallerPair,
					calleePair: tapCalleePair,
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

	// ── media anchor ─────────────────────────────────────────────────────────
	epHost, epPort, err := parseMedia(call.inbound.offerSDP)
	if err != nil {
		slog.Error("parse inbound SDP", "err", err)
		_ = call.inbound.session.Respond(488, "Not Acceptable Here", nil)
		releasePendingTaps(call, e.ports)
		call.teardown("bad inbound SDP")
		return
	}

	epPair, err := e.ports.Acquire()
	if err != nil {
		slog.Error("acquire endpoint media ports", "err", err)
		_ = call.inbound.session.Respond(503, "Service Unavailable", nil)
		releasePendingTaps(call, e.ports)
		call.teardown("port exhaustion")
		return
	}
	pbxPair, err := e.ports.Acquire()
	if err != nil {
		e.ports.Release(epPair)
		slog.Error("acquire pbx media ports", "err", err)
		_ = call.inbound.session.Respond(503, "Service Unavailable", nil)
		releasePendingTaps(call, e.ports)
		call.teardown("port exhaustion")
		return
	}

	epSide, err := newAnchorSide(e.mediaHost, epPair)
	if err != nil {
		e.ports.Release(epPair)
		e.ports.Release(pbxPair)
		slog.Error("bind endpoint media sockets", "err", err)
		_ = call.inbound.session.Respond(500, "Server Error", nil)
		releasePendingTaps(call, e.ports)
		call.teardown("media bind failed")
		return
	}
	pbxSide, err := newAnchorSide(e.mediaHost, pbxPair)
	if err != nil {
		epSide.close()
		e.ports.Release(epPair)
		e.ports.Release(pbxPair)
		slog.Error("bind pbx media sockets", "err", err)
		_ = call.inbound.session.Respond(500, "Server Error", nil)
		releasePendingTaps(call, e.ports)
		call.teardown("media bind failed")
		return
	}

	epSide.setRemote(
		&net.UDPAddr{IP: net.ParseIP(epHost), Port: epPort},
		&net.UDPAddr{IP: net.ParseIP(epHost), Port: epPort + 1},
	)

	// Register release hook so teardown cleans up even on mid-setup failure.
	// Tap pairs are released here too (they are on mediaSess.taps after registration below).
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

	pbxOffer, err := rewriteToAnchor(call.inbound.offerSDP, e.mediaHost, pbxPair.RTP)
	if err != nil {
		slog.Error("rewrite SDP for PBX", "err", err)
		_ = call.inbound.session.Respond(500, "Server Error", nil)
		call.teardown("rewrite sdp: " + err.Error())
		return
	}

	// ── pbx leg ───────────────────────────────────────────────────────────────
	var pbxURI sip.Uri
	if err := sip.ParseUri(e.cfg.NextHop, &pbxURI); err != nil {
		slog.Error("parse pbx URI", "uri", e.cfg.NextHop, "err", err)
		_ = call.inbound.session.Respond(500, "Server Error", nil)
		call.teardown("bad pbx URI")
		return
	}

	pbxLegID := newLegID()
	pbxHeaders := append(call.forwardHeaders(),
		sip.NewHeader("X-Sequencer-Call-Id", call.id),
		sip.NewHeader("X-Sequencer-Leg-Id", pbxLegID))
	pbxSess, err := e.dialogCliCache.Invite(ctx, pbxURI, pbxOffer, pbxHeaders...)
	if err != nil {
		e.metrics.TerminatingHopFailure()
		slog.Error("originate pbx leg", "uri", e.cfg.NextHop, "err", err)
		_ = call.inbound.session.Respond(503, "Service Unavailable", nil)
		call.teardown("pbx leg originate failed")
		return
	}
	call.mu.Lock()
	call.pbxLeg = &OutboundLeg{
		role:      rolePBX,
		targetURI: e.cfg.NextHop,
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
		call.teardown(fmt.Sprintf("pbx leg failed: %v", pbxErr))
		return
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
		_ = call.inbound.session.Respond(488, "Not Acceptable Here", nil)
		call.teardown("bad pbx answer SDP")
		return
	}
	pbxSide.setRemote(
		&net.UDPAddr{IP: net.ParseIP(pbxHost), Port: pbxRTPPort},
		&net.UDPAddr{IP: net.ParseIP(pbxHost), Port: pbxRTPPort + 1},
	)

	// Rewrite PBX answer for the endpoint: codec from PBX, address from sequencer.
	epAnswerSDP, err := rewriteToAnchor(pbxAnswerRaw, e.mediaHost, epPair.RTP)
	if err != nil {
		slog.Error("rewrite SDP for endpoint", "err", err)
		_ = call.inbound.session.Respond(500, "Server Error", nil)
		call.teardown("rewrite sdp: " + err.Error())
		return
	}

	// answer the endpoint with the anchored SDP, relaying the PBX 200's headers
	if err := call.inbound.session.Respond(200, "OK", epAnswerSDP,
		relayableResponseHeaders(pbxResp, epAnswerSDP)...); err != nil {
		slog.Error("answer endpoint", "err", err)
		call.teardown("answer endpoint failed")
		return
	}

	e.metrics.ObserveSequencingLatency(time.Since(start))

	call.mu.Lock()
	if canTransition(call.state, stateEstablished) {
		call.state = stateEstablished
	}
	// Register all pending taps on the media session before relay starts.
	for _, pt := range call.pendingTaps {
		mediaSess.addTap(pt.tap)
	}
	call.pendingTaps = nil
	call.mu.Unlock()

	go mediaSess.relay(ctx)

	<-ctx.Done()
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
