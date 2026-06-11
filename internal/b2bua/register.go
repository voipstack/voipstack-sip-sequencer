package b2bua

import (
	"fmt"
	"log/slog"

	"github.com/emiago/sipgo/sip"
)

// handleRegister is the registration edge. The sequencer is not the authoritative
// registrar: on REGISTER it records the arriving flow (source address + transport),
// mints a signed flow token, inserts a Path (RFC 3327) addressing this sequencer with
// that token, forwards the REGISTER to the upstream registrar (cfg.NextHop), and
// relays the registrar's response verbatim — so auth challenges (401/407) and the
// 200 with its reflected Path round-trip end to end. Outbound markers (;ob, reg-id,
// +sip.instance) ride along unchanged for the registrar to consume.
func (e *Engine) handleRegister(req *sip.Request, tx sip.ServerTransaction) {
	flow := Flow{Addr: req.Source(), Transport: req.Transport()}
	token := mintFlowToken(flow, e.flowSecret)
	// The Path addresses this sequencer at its signaling address; the flow's own
	// transport (ws/wss) is carried inside the signed token, not on the Path. The Path
	// transport must describe how the upstream registrar reaches the sequencer, which
	// is its default UDP signaling — so it is omitted and defaults to udp.
	pathValue := fmt.Sprintf("<sip:%s@%s:%d;lr>", token, e.pathHost, e.pathPort)

	var nextHop sip.Uri
	if err := sip.ParseUri(e.cfg.NextHop.URI, &nextHop); err != nil {
		slog.Error("register: parse next-hop URI", "nextHop", e.cfg.NextHop.URI, "err", err)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 500, "Internal Server Error", nil))
		return
	}

	slog.Info("register forwarding", "method", req.Method, "callID", callIDValue(req), "nextHop", nextHop.HostPort())

	insertPath := func(fwd *sip.Request) {
		fwd.AppendHeader(sip.NewHeader("Path", pathValue))
	}
	e.forwardAndRelay(req, tx, nextHop.HostPort(), e.nextHopTransport(),
		proxyFault{502, "Bad Gateway"}, proxyFault{503, "Service Unavailable"},
		insertPath)
}
