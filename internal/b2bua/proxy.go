package b2bua

import (
	"fmt"
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
	origMF := req.MaxForwards()
	if origMF != nil && origMF.Val() == 0 {
		slog.Info("proxy rejected: max-forwards exhausted", append([]any{"method", req.Method}, cidArgs...)...)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 483, "Too Many Hops", nil))
		return
	}

	// Clone so ClientRequestAddVia and the prepare hooks do not mutate the inbound request.
	fwd := req.Clone()
	fwd.SetBody(req.Body())

	// Decrement Max-Forwards on the clone (or set 70 if absent).
	fwd.RemoveHeader("Max-Forwards")
	newMF := sip.MaxForwardsHeader(70)
	if origMF != nil {
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

	// Forward; sipgo prepends a proxy Via via ClientRequestAddVia.
	ctx := e.runCtx
	clientTx, err := e.cli.TransactionRequest(ctx, fwd, sipgo.ClientRequestAddVia)
	if err != nil {
		slog.Error(fmt.Sprintf("proxy %s to %q: %v", req.Method, destination, err))
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
			// Strip the proxy Via we added before relaying to the originator.
			vias := res.GetHeaders("Via")
			res.RemoveHeader("Via")
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

// routeToFlow recognizes an inbound request whose top Route is this sequencer's Path
// carrying a valid flow token, pops that Route, and forwards the request to the
// webphone's recorded flow address — letting sipgo's connection reuse deliver it over
// the existing WebSocket. It returns false when the request is not a self-Route flow
// request (no Route, a foreign host, or a token that fails MAC verification), so
// normal handling proceeds. It forwards only to an address recovered from a verified
// token; a dropped flow yields 480 (no new connection is dialed).
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

	flow, err := parseFlowToken(rh.Address.User, e.flowSecret)
	if err != nil {
		// Forged or foreign token: never forward to an unverified address.
		slog.Warn("flow route rejected: invalid token", "host", rh.Address.HostPort())
		return false
	}

	// Pop the top Route (self-route removal, RFC 3261 §16.6) on the forwarded clone,
	// preserving any remaining Route headers.
	popTopRoute := func(fwd *sip.Request) {
		routes := fwd.GetHeaders("Route")
		fwd.RemoveHeader("Route")
		for _, r := range routes[1:] {
			fwd.AppendHeader(r)
		}
	}

	slog.Info("flow route forwarding", "method", req.Method, "callID", callIDValue(req), "flow", flow.Addr)
	e.forwardAndRelay(req, tx, flow.Addr, strings.ToLower(flow.Transport),
		proxyFault{480, "Temporarily Unavailable"}, proxyFault{480, "Temporarily Unavailable"},
		popTopRoute)
	return true
}

// callIDValue returns req's Call-ID value, or "" when absent.
func callIDValue(req *sip.Request) string {
	if h := req.CallID(); h != nil {
		return h.Value()
	}
	return ""
}
