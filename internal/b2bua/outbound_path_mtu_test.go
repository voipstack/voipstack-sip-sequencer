package b2bua

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// ── wsCaller: a WebSocket SIP client that originates calls ─────────────────────
//
// A WS (TCP-framed) inbound leg can carry a request far larger than the UDP MTU, so
// it is how a real large INVITE (e.g. a WebRTC offer with many ICE candidates) reaches
// the sequencer before being routed back onto a UDP endpoint's flow.
type wsCaller struct {
	cli    *sipgo.Client
	target string
	wsAddr string
}

func newWSCaller(t *testing.T, engineWSAddr string) *wsCaller {
	t.Helper()

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("caller UA: %v", err)
	}
	if _, err := sipgo.NewServer(ua); err != nil {
		t.Fatalf("caller server: %v", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("caller client: %v", err)
	}
	t.Cleanup(func() { _ = ua.Close() })

	return &wsCaller{cli: cli, target: "sip:" + engineWSAddr + ";transport=ws", wsAddr: engineWSAddr}
}

// inviteWithRoute sends an INVITE carrying the given Route header and body over the ws
// flow, returning the first final response.
func (c *wsCaller) inviteWithRoute(ctx context.Context, routeValue string, body []byte) (*sip.Response, error) {
	var uri sip.Uri
	if err := sip.ParseUri(c.target, &uri); err != nil {
		return nil, err
	}
	req := sip.NewRequest(sip.INVITE, uri)
	req.AppendHeader(sip.NewHeader("Route", routeValue))
	req.AppendHeader(sip.NewHeader("Contact", "<sip:caller@example.invalid;transport=ws>"))
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(body)
	// Force delivery over the ws flow. With a Route present, sipgo would otherwise pick
	// the transport from the Route's URI (the Path, which defaults to udp) and try to
	// send this oversized INVITE over UDP itself — the very limit under test belongs to
	// the sequencer's *forward*, not the caller's inbound leg.
	req.SetTransport("ws")
	req.SetDestination(c.wsAddr)

	tx, err := c.cli.TransactionRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	defer tx.Terminate()
	for {
		select {
		case res, ok := <-tx.Responses():
			if !ok {
				return nil, context.Canceled
			}
			if res.StatusCode >= 200 {
				return res, nil
			}
		case <-tx.Done():
			return nil, context.Canceled
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// oversizedSDP returns an SDP body large enough that the INVITE the sequencer forwards
// onto the flow exceeds sipgo's UDP MTU threshold (sip.UDPMTUSize-200 = 1300 bytes).
func oversizedSDP() []byte {
	pad := strings.Repeat("x", 3000)
	return []byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\n" +
		"t=0 0\r\nm=audio 5000 RTP/AVP 0\r\na=label:" + pad + "\r\n")
}

// expectNoUDPInvite fails if the udp phone receives an INVITE within window.
func expectNoUDPInvite(t *testing.T, p *udpPhone, window time.Duration) {
	t.Helper()
	select {
	case <-p.invites:
		t.Fatal("phone unexpectedly received the oversized INVITE")
	case <-time.After(window):
	}
}

// LIMITATION (RFC 3261 §18.1.1): routing an inbound INVITE back onto a *UDP* flow fails
// when the forwarded request exceeds the path MTU. sipgo refuses to write a UDP packet
// larger than sip.UDPMTUSize-200 (1300 bytes), returning sip.ErrUDPMTUCongestion
// ("size of packet larger than MTU") instead of switching to a congestion-controlled
// transport — sipgo leaves that TCP fallback to the application, and the sequencer does
// not yet do it. So a large INVITE (here a 3 KB WebRTC-style SDP) arriving over ws and
// routed onto a UDP endpoint's flow never reaches the phone; the caller gets the flow
// path's 480.
//
// This test pins that current behavior explicitly. When TCP fallback is implemented it
// MUST be updated to expect the phone to ring (over TCP) instead of a 480.
func TestLargeInviteToUDPFlowHitsMTULimit(t *testing.T) {
	registrar := newFakeRegistrar(t)

	plainAddr := freeAddr(t)
	wsAddr := freeAddr(t)
	_ = startEngineWS(t, wsConfig(plainAddr, wsAddr, registrar.sipURI(), registrar.sipURI()))

	// A plain-UDP phone registers, so its recorded flow transport is udp.
	phone := newUDPPhone(t, plainAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	phone.register(t, ctx)

	reg := registrar.waitRegister(t, 3*time.Second)
	pathValue := pathOf(t, reg)

	// A ws caller (its TCP framing carries the oversized body fine) places the call via
	// the recorded Path. The forwarded INVITE onto the udp flow blows the MTU.
	body := oversizedSDP()
	if len(body) <= sip.UDPMTUSize-200 {
		t.Fatalf("test body %d bytes must exceed the UDP MTU threshold %d", len(body), sip.UDPMTUSize-200)
	}
	caller := newWSCaller(t, wsAddr)
	res, err := caller.inviteWithRoute(ctx, pathValue, body)
	if err != nil {
		t.Fatalf("ws caller invite: %v", err)
	}

	// Current behavior: the oversized UDP forward is rejected, the phone never rings, and
	// the caller receives the flow path's onSendErr (480).
	if res.StatusCode != 480 {
		t.Fatalf("expected 480 (oversized UDP forward rejected), got %d %s", res.StatusCode, res.Reason)
	}
	expectNoUDPInvite(t, phone, 300*time.Millisecond)
}
