package b2bua

import (
	"context"
	"log/slog"
	"net"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// handleRefer handles an in-dialog REFER. It re-points the endpoint-facing media
// to the transfer target while leaving appLegs and pbxLeg untouched. The
// correlation call_id is never reissued.
func (e *Engine) handleRefer(req *sip.Request, tx sip.ServerTransaction) {
	dss, err := e.dialogSrvCache.MatchDialogRequest(req)
	if err != nil {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil))
		return
	}

	call, ok := e.calls.getByDialog(dss.ID)
	if !ok {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil))
		return
	}

	call.mu.Lock()
	if call.state != stateEstablished {
		call.mu.Unlock()
		_ = tx.Respond(sip.NewResponseFromRequest(req, 403, "Forbidden", nil))
		return
	}
	// A webphone (secured) call has no plain endpointSide to re-point; REFER on the
	// DTLS-SRTP leg is not supported yet. Reject without disrupting the established media.
	if call.media == nil || call.media.endpointSide == nil {
		call.mu.Unlock()
		_ = tx.Respond(sip.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil))
		return
	}
	media := call.media
	epPort := call.media.endpointSide.localRTPPort
	pbxAnswerSDP := copyBody(call.pbxLeg.answerSDP)
	inboundSession := call.inbound.session
	call.mu.Unlock()

	// Respond 202 Accepted immediately so the referrer does not timeout.
	_ = tx.Respond(sip.NewResponseFromRequest(req, 202, "Accepted", nil))

	referToHdr := req.GetHeader("Refer-To")
	if referToHdr == nil {
		slog.Warn("REFER: missing Refer-To header", "callID", call.id)
		e.sendReferNotify(inboundSession, "SIP/2.0 400 Bad Request")
		return
	}
	var targetURI sip.Uri
	if err := sip.ParseUri(referToHdr.Value(), &targetURI); err != nil {
		slog.Warn("REFER: parse Refer-To", "callID", call.id, "err", err)
		e.sendReferNotify(inboundSession, "SIP/2.0 400 Bad Request")
		return
	}

	// Offer the transfer target on the existing endpoint anchor port.
	anchoredOffer, err := rewriteToAnchor(pbxAnswerSDP, e.mediaHost, epPort)
	if err != nil {
		slog.Error("REFER: build offer", "callID", call.id, "err", err)
		e.sendReferNotify(inboundSession, "SIP/2.0 500 Server Error")
		return
	}

	ctx, cancel := context.WithTimeout(e.runCtx, e.legTimeout)
	defer cancel()

	legHeaders := append(call.forwardHeaders(),
		sip.NewHeader("X-Sequencer-Call-Id", call.id))
	targetSess, err := e.dialogCliCache.Invite(ctx, targetURI, anchoredOffer, legHeaders...)
	if err != nil {
		slog.Warn("REFER: originate to target", "callID", call.id, "err", err)
		e.sendReferNotify(inboundSession, "SIP/2.0 503 Service Unavailable")
		return
	}

	legCtx, legCancel := context.WithTimeout(ctx, e.legTimeout)
	targetErr := targetSess.WaitAnswer(legCtx, sipgo.AnswerOptions{})
	legCancel()

	if targetErr != nil {
		slog.Warn("REFER: transfer target answer failed", "callID", call.id, "err", targetErr)
		byeCtx, byeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = targetSess.Bye(byeCtx)
		byeCancel()
		e.sendReferNotify(inboundSession, "SIP/2.0 503 Service Unavailable")
		return
	}

	targetAnswerSDP := copyBody(targetSess.InviteResponse.Body())
	if err := targetSess.Ack(ctx); err != nil {
		slog.Warn("REFER: ACK target", "callID", call.id, "err", err)
	}

	targetHost, targetRTPPort, err := parseMedia(targetAnswerSDP)
	if err != nil {
		slog.Error("REFER: parse target SDP", "callID", call.id, "err", err)
		byeCtx, byeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = targetSess.Bye(byeCtx)
		byeCancel()
		e.sendReferNotify(inboundSession, "SIP/2.0 488 Not Acceptable Here")
		return
	}

	newTargetRTP := &net.UDPAddr{IP: net.ParseIP(targetHost), Port: targetRTPPort}
	newTargetRTCP := &net.UDPAddr{IP: net.ParseIP(targetHost), Port: targetRTPPort + 1}

	call.mu.Lock()
	media.reanchor(media.endpointSide, newTargetRTP, newTargetRTCP)
	call.transferTarget = targetSess
	call.mu.Unlock()

	// Register teardown on transfer target dialog end.
	targetSess.OnState(func(s sip.DialogState) {
		if s == sip.DialogStateEnded {
			call.teardown("transfer target dialog ended")
		}
	})

	// Notify referrer of success, then BYE them (transfer complete).
	e.sendReferNotify(inboundSession, "SIP/2.0 200 OK")

	byeCtx, byeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer byeCancel()
	_ = inboundSession.Bye(byeCtx)
}

// sendReferNotify sends a minimal NOTIFY with message/sipfrag body to the referrer.
// Failures are logged and ignored — the call is not affected.
func (e *Engine) sendReferNotify(dss *sipgo.DialogServerSession, sipfrag string) {
	contact := dss.InviteRequest.Contact()
	if contact == nil {
		return
	}

	ctx, cancel := context.WithTimeout(e.runCtx, 5*time.Second)
	defer cancel()

	notify := sip.NewRequest(sip.NOTIFY, contact.Address)
	notify.AppendHeader(sip.NewHeader("Event", "refer"))
	notify.AppendHeader(sip.NewHeader("Subscription-State", "terminated;reason=noresource"))
	notify.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag"))
	notify.SetBody([]byte(sipfrag))

	tx, err := e.cli.TransactionRequest(ctx, notify)
	if err != nil {
		slog.Warn("REFER: send NOTIFY", "err", err)
		return
	}
	defer tx.Terminate()
	select {
	case <-tx.Responses():
	case <-tx.Done():
	case <-ctx.Done():
	}
}
