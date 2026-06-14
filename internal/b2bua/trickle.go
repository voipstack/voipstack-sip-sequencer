package b2bua

import (
	"log/slog"

	"github.com/emiago/sipgo/sip"
)

// trickleLeg is the consumer-side view of a media leg that accepts trickled ICE
// candidates. Only the secured (WebRTC) leg satisfies it; a plain leg fails the type
// assertion and is not trickled.
type trickleLeg interface {
	AddRemoteCandidate(candidate string) error
	EndOfRemoteCandidates() error
}

// handleInfo intercepts an in-dialog INFO and consumes it only when it is a trickle-ICE
// fragment (RFC 8840) on a matched call whose endpoint leg is secured. Every other INFO
// — DTMF and any trickle on a plain/proxied dialog — is forwarded to cfg.NextHop by
// proxyUnmanaged exactly as before. An unknown dialog gets 481. Ingestion is
// best-effort: a malformed/empty fragment or a per-candidate error is logged and still
// answered 200 OK, never disrupting a leg that is coming up.
func (e *Engine) handleInfo(req *sip.Request, tx sip.ServerTransaction) {
	// Content-type gate: only trickle-ICE fragments are ours; everything else proxies.
	ctHeader := req.GetHeader("Content-Type")
	if ctHeader == nil || !isTrickleContentType(ctHeader.Value()) {
		e.proxyUnmanaged(req, tx)
		return
	}

	// Dialog match: a trickle on a dialog we do not own is not ours to consume.
	dss, err := e.dialogSrvCache.MatchDialogRequest(req)
	if err != nil {
		e.proxyUnmanaged(req, tx)
		return
	}

	call, ok := e.calls.getByDialog(dss.ID)
	if !ok {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 481, "Call/Transaction Does Not Exist", nil))
		return
	}

	// Leg capability: read media under the lock, then type-assert outside it. A nil
	// media/endpointLeg or a plain leg fails the assertion and is proxied, not consumed.
	call.mu.Lock()
	media := call.media
	call.mu.Unlock()
	var leg trickleLeg
	if media != nil {
		leg, ok = media.endpointLeg.(trickleLeg)
	}
	if !ok || leg == nil {
		e.proxyUnmanaged(req, tx)
		return
	}

	// Ingest best-effort: feed every candidate, then end-of-candidates. Errors are
	// logged and skipped; the leg is never disrupted.
	frag := parseTrickleFragment(req.Body())
	callID := callIDValue(req)
	for _, c := range frag.Candidates {
		if err := leg.AddRemoteCandidate(c); err != nil {
			slog.Warn("trickle add candidate", "callID", callID, "err", err)
		}
	}
	if frag.EndOfCandidates {
		if err := leg.EndOfRemoteCandidates(); err != nil {
			slog.Warn("trickle end of candidates", "callID", callID, "err", err)
		}
	}

	_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
}
