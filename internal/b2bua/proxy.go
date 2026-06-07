package b2bua

import (
	"fmt"
	"log/slog"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// proxyUnmanaged stateless-forwards one unmanaged SIP request to cfg.NextHop and
// relays every response back to the originator. It never touches the call path.
func (e *Engine) proxyUnmanaged(req *sip.Request, tx sip.ServerTransaction) {
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

	// Clone so ClientRequestAddVia does not mutate the inbound request.
	fwd := req.Clone()
	fwd.SetBody(req.Body())

	// Decrement Max-Forwards on the clone (or set 70 if absent).
	fwd.RemoveHeader("Max-Forwards")
	newMF := sip.MaxForwardsHeader(70)
	if origMF != nil {
		newMF = sip.MaxForwardsHeader(origMF.Val() - 1)
	}
	fwd.AppendHeader(&newMF)

	// Determine next-hop address.
	var nextHop sip.Uri
	if err := sip.ParseUri(e.cfg.NextHop, &nextHop); err != nil {
		slog.Error("proxy: parse next-hop URI", "nextHop", e.cfg.NextHop, "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil))
		return
	}
	fwd.SetDestination(nextHop.HostPort())

	// Forward; sipgo prepends a proxy Via via ClientRequestAddVia.
	ctx := e.runCtx
	clientTx, err := e.cli.TransactionRequest(ctx, fwd, sipgo.ClientRequestAddVia)
	if err != nil {
		slog.Error(fmt.Sprintf("proxy %s to %q: %v", req.Method, e.cfg.NextHop, err))
		_ = tx.Respond(sip.NewResponseFromRequest(req, 502, "Bad Gateway", nil))
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
				slog.Info("proxy forwarded", append([]any{"method", req.Method, "nextHop", nextHop.HostPort(), "status", res.StatusCode}, cidArgs...)...)
			}
			_ = tx.Respond(res)
			if res.StatusCode >= 200 {
				return
			}
		case <-clientTx.Done():
			// Transport failure or timer expiry with no final response.
			slog.Warn("proxy: next-hop gave no final response", append([]any{"method", req.Method, "nextHop", nextHop.HostPort()}, cidArgs...)...)
			_ = tx.Respond(sip.NewResponseFromRequest(req, 503, "Service Unavailable", nil))
			return
		case <-ctx.Done():
			slog.Warn("proxy: context cancelled", append([]any{"method", req.Method, "nextHop", nextHop.HostPort()}, cidArgs...)...)
			_ = tx.Respond(sip.NewResponseFromRequest(req, 408, "Request Timeout", nil))
			return
		}
	}
}
