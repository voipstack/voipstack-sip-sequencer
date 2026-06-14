package b2bua

import (
	"context"
	"log/slog"
	"net"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// handleReInvite propagates an in-dialog re-INVITE to the PBX leg and re-anchors
// media. It never re-runs the application chain or touches appLegs.
func (e *Engine) handleReInvite(call *Call, inbound *sipgo.DialogServerSession, req *sip.Request, tx sip.ServerTransaction) {
	call.mu.Lock()
	if call.state != stateEstablished {
		call.mu.Unlock()
		_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil))
		return
	}
	// A webphone (secured) call has no plain endpointSide to re-anchor; re-INVITE on the
	// DTLS-SRTP leg is not supported yet. Reject without disrupting the established media.
	if call.media == nil || call.media.endpointSide == nil {
		call.mu.Unlock()
		_ = tx.Respond(sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil))
		return
	}
	// Snapshot everything needed; unlock before any blocking I/O.
	pbxSess := call.pbxLeg.session
	media := call.media
	epPort := call.media.endpointSide.localRTPPort
	pbxPort := call.media.pbxSide.localRTPPort
	pbxAnswerSDP := copyBody(call.pbxLeg.answerSDP)
	call.mu.Unlock()

	body := req.Body()
	if len(body) == 0 {
		// Session refresh: re-answer with the current anchored endpoint SDP.
		currentEpSDP, err := rewriteToAnchor(pbxAnswerSDP, e.mediaHost, epPort)
		if err != nil {
			slog.Error("re-INVITE session refresh: rewrite", "callID", call.id, "err", err)
			_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Server Error", nil))
			return
		}
		res := sip.NewResponseFromRequest(req, 200, "OK", currentEpSDP)
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		_ = tx.Respond(res)
		return
	}

	// Parse the new endpoint offer.
	epHost, epRTPPort, err := parseMedia(body)
	if err != nil {
		slog.Warn("re-INVITE: parse endpoint offer", "callID", call.id, "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil))
		return
	}

	// Build anchored re-offer for the PBX (direction attributes pass through verbatim).
	pbxReOffer, err := rewriteToAnchor(body, e.mediaHost, pbxPort)
	if err != nil {
		slog.Error("re-INVITE: rewrite to PBX anchor", "callID", call.id, "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Server Error", nil))
		return
	}

	// Send in-dialog re-INVITE to PBX.
	contactURI := pbxSess.InviteResponse.Contact().Address
	reInvite := sip.NewRequest(sip.INVITE, contactURI)
	reInvite.SetBody(pbxReOffer)
	reInvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))

	ctx, cancel := context.WithTimeout(e.runCtx, e.legTimeout)
	defer cancel()

	pbxRes, err := pbxSess.Do(ctx, reInvite)
	if err != nil {
		slog.Warn("re-INVITE: PBX transaction error", "callID", call.id, "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
		return
	}
	if pbxRes.StatusCode >= 300 {
		status := mapFailureStatus(failureReject, pbxRes.StatusCode)
		slog.Warn("re-INVITE: PBX non-2xx", "callID", call.id, "status", pbxRes.StatusCode)
		pbxErrBody := copyBody(pbxRes.Body())
		errRes := sip.NewResponseFromRequest(req, status, pbxRes.Reason, pbxErrBody)
		for _, h := range relayableResponseHeaders(pbxRes, pbxErrBody) {
			errRes.AppendHeader(h)
		}
		_ = tx.Respond(errRes)
		return
	}

	if err := pbxSess.Ack(ctx); err != nil {
		slog.Warn("re-INVITE: ACK PBX", "callID", call.id, "err", err)
	}

	pbxAnswerRaw := copyBody(pbxRes.Body())
	pbxHost, pbxRTPPort, err := parseMedia(pbxAnswerRaw)
	if err != nil {
		slog.Error("re-INVITE: parse PBX answer", "callID", call.id, "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil))
		return
	}

	// Atomically re-anchor both sides; the relay goroutines pick up the new
	// addresses on their next packet without restart.
	newEpRTP := &net.UDPAddr{IP: net.ParseIP(epHost), Port: epRTPPort}
	newEpRTCP := &net.UDPAddr{IP: net.ParseIP(epHost), Port: epRTPPort + 1}
	newPbxRTP := &net.UDPAddr{IP: net.ParseIP(pbxHost), Port: pbxRTPPort}
	newPbxRTCP := &net.UDPAddr{IP: net.ParseIP(pbxHost), Port: pbxRTPPort + 1}

	call.mu.Lock()
	media.reanchor(media.endpointSide, newEpRTP, newEpRTCP)
	media.reanchor(media.pbxSide, newPbxRTP, newPbxRTCP)
	call.pbxLeg.answerSDP = pbxAnswerRaw
	call.mu.Unlock()

	// Answer endpoint with PBX answer rewritten onto the endpoint anchor port.
	epAnswerSDP, err := rewriteToAnchor(pbxAnswerRaw, e.mediaHost, epPort)
	if err != nil {
		slog.Error("re-INVITE: rewrite EP answer", "callID", call.id, "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Server Error", nil))
		return
	}

	okRes := sip.NewResponseFromRequest(req, 200, "OK", epAnswerSDP)
	for _, h := range relayableResponseHeaders(pbxRes, epAnswerSDP) {
		okRes.AppendHeader(h)
	}
	_ = tx.Respond(okRes)
}
