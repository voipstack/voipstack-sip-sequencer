package b2bua

import (
	"log/slog"
	"strings"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// proxyFault is the final response forwardAndRelay sends when a forward cannot
// complete: a transport error on send (onSendErr), or a destination that produced no
// final response (onNoFinal).
type proxyFault struct {
	code   int
	reason string
}

// proxyUnmanaged stateless-forwards one unmanaged SIP request to cfg.NextHop and
// relays every response back to the originator. It never touches the call path.
func (e *Engine) proxyUnmanaged(req *sip.Request, tx sip.ServerTransaction) {
	var nextHop sip.Uri
	if err := sip.ParseUri(e.cfg.NextHop.URI, &nextHop); err != nil {
		slog.Error("proxy: parse next-hop URI", "nextHop", e.cfg.NextHop.URI, "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil))
		return
	}
	e.forwardAndRelay(req, tx, nextHop.HostPort(), e.nextHopTransport(),
		proxyFault{502, "Bad Gateway"}, proxyFault{503, "Service Unavailable"})
}

// nextHopTransport is the SIP transport keyword the forwarded request must carry to
// reach cfg.NextHop, independent of the transport the inbound request arrived on.
func (e *Engine) nextHopTransport() string {
	switch e.cfg.NextHop.Transport {
	case config.TransportTLS:
		return "tls"
	case config.TransportTCP:
		return "tcp"
	default:
		return "udp"
	}
}

// forwardAndRelay clones req, decrements Max-Forwards, applies any prepare hooks to
// the clone (insert a Path, pop a self-Route), sends it to destination, and relays
// every response back to the originator — stripping the single proxy Via it added.
// It is the shared forward path behind proxyUnmanaged, handleRegister, and
// routeToFlow. onSendErr answers a transport failure on send; onNoFinal answers a
// destination that gave no final response.
func (e *Engine) forwardAndRelay(req *sip.Request, tx sip.ServerTransaction, destination, transport string, onSendErr, onNoFinal proxyFault, prepare ...func(*sip.Request)) {
	var cidArgs []any
	if h := req.CallID(); h != nil {
		cidArgs = []any{"callID", h.Value()}
	}

	// Reject loops: Max-Forwards: 0 → 483.
	if origMF := req.MaxForwards(); origMF != nil && origMF.Val() == 0 {
		slog.Info("proxy rejected: max-forwards exhausted", append([]any{"method", req.Method}, cidArgs...)...)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 483, "Too Many Hops", nil))
		return
	}

	fwd := buildForward(req, destination, transport, prepare...)

	// Forward; sipgo prepends a proxy Via via ClientRequestAddVia.
	ctx := e.runCtx
	clientTx, err := e.cli.TransactionRequest(ctx, fwd, sipgo.ClientRequestAddVia)
	if err != nil {
		logForwardError(destination, fwd, err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, onSendErr.code, onSendErr.reason, nil))
		return
	}
	defer clientTx.Terminate()

	for {
		select {
		case res, ok := <-clientTx.Responses():
			if !ok {
				return
			}
			// Strip the proxy Via we added (RFC 3261 §16.7) before relaying to the
			// originator, leaving the originator's Via(s) intact and once each.
			// sipgo's RemoveHeader removes only the FIRST matching header, so removing
			// the whole list takes a loop; removing just once would keep the
			// originator's Via and then re-appending vias[1:] would duplicate it —
			// a strict WS client (jsSIP) drops a response with more than one Via.
			vias := res.GetHeaders("Via")
			for res.RemoveHeader("Via") {
			}
			for _, v := range vias[1:] {
				res.AppendHeader(v)
			}
			if res.StatusCode >= 200 {
				slog.Info("proxy forwarded", append([]any{"method", req.Method, "nextHop", destination, "status", res.StatusCode}, cidArgs...)...)
			}
			_ = tx.Respond(res)
			if res.StatusCode >= 200 {
				return
			}
		case <-clientTx.Done():
			// Transport failure or timer expiry with no final response.
			slog.Warn("proxy: next-hop gave no final response", append([]any{"method", req.Method, "nextHop", destination}, cidArgs...)...)
			_ = tx.Respond(sip.NewResponseFromRequest(req, onNoFinal.code, onNoFinal.reason, nil))
			return
		case <-ctx.Done():
			slog.Warn("proxy: context cancelled", append([]any{"method", req.Method, "nextHop", destination}, cidArgs...)...)
			_ = tx.Respond(sip.NewResponseFromRequest(req, 408, "Request Timeout", nil))
			return
		}
	}
}

// buildForward clones req for proxying to destination over transport. It copies the
// body, decrements Max-Forwards (RFC 3261 §16.6, defaulting to 70 when absent), runs
// the prepare hooks (insert a Path/Record-Route, pop a self-Route), forces the
// outbound transport, and pins the destination. It does not send — both the
// transaction (forwardAndRelay) and stateless (forwardStateless) forward paths build
// their clone here so the two share one definition of "a forwarded request".
func buildForward(req *sip.Request, destination, transport string, prepare ...func(*sip.Request)) *sip.Request {
	fwd := req.Clone()
	fwd.SetBody(req.Body())

	fwd.RemoveHeader("Max-Forwards")
	newMF := sip.MaxForwardsHeader(70)
	if origMF := req.MaxForwards(); origMF != nil {
		newMF = sip.MaxForwardsHeader(origMF.Val() - 1)
	}
	fwd.AppendHeader(&newMF)

	for _, p := range prepare {
		p(fwd)
	}

	// Force the outbound transport to the destination's, independent of the transport
	// the request arrived on — otherwise a ws-inbound REGISTER/INVITE would leak
	// transport=ws onto a UDP/TCP next-hop and mis-dial.
	if transport != "" {
		fwd.Recipient = withTransport(fwd.Recipient, transport)
		fwd.SetTransport(strings.ToUpper(transport))
	}
	fwd.SetDestination(destination)
	return fwd
}

// forwardStateless forwards an ACK for a 2xx. An ACK is a standalone message with no
// response, so it cannot ride forwardAndRelay's request/response loop: the proxy
// writes it on and is done. sipgo prepends the proxy Via via ClientRequestAddVia.
func (e *Engine) forwardStateless(req *sip.Request, destination, transport string, prepare ...func(*sip.Request)) {
	fwd := buildForward(req, destination, transport, prepare...)
	if err := e.cli.WriteRequest(fwd, sipgo.ClientRequestAddVia); err != nil {
		logForwardError(destination, fwd, err)
	}
}

// logForwardError logs a failed forward together with the full SIP message that could
// not be sent and its byte size — so a transport failure such as an over-MTU UDP write
// ("size of packet larger than MTU") is diagnosable from the actual on-wire bytes, not
// just the error string. sipgo has already applied the proxy Via to fwd by the time the
// write fails, so the dump reflects what it tried to put on the wire.
func logForwardError(destination string, fwd *sip.Request, err error) {
	raw := fwd.String()
	slog.Error("proxy forward failed",
		"method", fwd.Method, "dest", destination, "bytes", len(raw), "err", err, "raw", raw)
}

// routeToFlow handles a request whose top Route is this sequencer's Path carrying a
// valid flow token. It pops that self-Route and proxies the request along the
// webphone's flow — letting sipgo's connection reuse deliver it over the existing
// WebSocket — so an inbound call and its in-dialog follow-ups (ACK, BYE, re-INVITE)
// all reach a webphone whose own Contact is unroutable.
//
// To stay in that dialog path, the initial INVITE toward the phone is Record-Routed
// with the same self-Route value, so the caller addresses every later in-dialog
// request back through here. A request arriving *from* the flow is the phone
// answering in-dialog (e.g. its own BYE): that one is proxied outward toward its
// Request-URI, not looped back onto the flow.
//
// It returns false when the request is not a self-Route flow request (no Route, a
// foreign host, or a token that fails to decode) so normal handling proceeds; it
// forwards only to an address recovered from the token, and a dropped flow yields 480
// (no new connection is dialed).
func (e *Engine) routeToFlow(req *sip.Request, tx sip.ServerTransaction) bool {
	top := req.GetHeader("Route")
	if top == nil {
		return false
	}
	rh, ok := top.(*sip.RouteHeader)
	if !ok {
		return false
	}
	if rh.Address.Host != e.pathHost || rh.Address.Port != e.pathPort {
		return false
	}

	flow, err := parseFlowToken(rh.Address.User)
	if err != nil {
		// Malformed token: never forward to an address we could not decode.
		slog.Warn("flow route rejected: invalid token", "host", rh.Address.HostPort())
		return false
	}

	selfRoute := rh.Clone()

	// Pop the top Route (self-route removal, RFC 3261 §16.6) on the forwarded clone,
	// preserving any remaining Route headers.
	prepare := []func(*sip.Request){func(fwd *sip.Request) {
		routes := fwd.GetHeaders("Route")
		fwd.RemoveHeader("Route")
		for _, r := range routes[1:] {
			fwd.AppendHeader(r)
		}
	}}

	// Direction: a request that arrived over the phone's own flow is the phone speaking
	// in-dialog, so it travels outward to its Request-URI; anything else is bound for
	// the phone and travels onto the flow.
	destination, transport := flow.Addr, strings.ToLower(flow.Transport)
	if req.Source() == flow.Addr {
		destination, transport = req.Recipient.HostPort(), ""
	} else if req.IsInvite() {
		// Keep the sequencer in the dialog so the caller's later in-dialog requests
		// return here — the phone's Contact is unroutable, so this is the only way the
		// dialog can continue. Reuse the consumed self-Route as the Record-Route.
		rr := &sip.RecordRouteHeader{Address: *selfRoute.Address.Clone()}
		prepare = append(prepare, func(fwd *sip.Request) { fwd.PrependHeader(rr) })
	}

	slog.Info("flow route forwarding", "method", req.Method, "callID", callIDValue(req), "dest", destination)
	if req.IsAck() {
		e.forwardStateless(req, destination, transport, prepare...)
		return true
	}
	e.forwardAndRelay(req, tx, destination, transport,
		proxyFault{480, "Temporarily Unavailable"}, proxyFault{480, "Temporarily Unavailable"},
		prepare...)
	return true
}

// callIDValue returns req's Call-ID value, or "" when absent.
func callIDValue(req *sip.Request) string {
	if h := req.CallID(); h != nil {
		return h.Value()
	}
	return ""
}
