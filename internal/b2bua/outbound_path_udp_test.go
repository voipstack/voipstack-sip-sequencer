package b2bua

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// ── udpPhone: a plain-UDP SIP client (hard-phone stand-in, e.g. Twinkle) ───────
//
// Unlike wsPhone it has a routable Contact: it serves inbound INVITEs on its own
// UDP socket. It registers over that socket so the source address the sequencer
// records as the flow is the phone's own listen address.
type udpPhone struct {
	ua      *sipgo.UserAgent
	cli     *sipgo.Client
	addr    string
	target  string
	invites chan *sip.Request
}

func newUDPPhone(t *testing.T, engineAddr string) *udpPhone {
	t.Helper()

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("phone UA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("phone server: %v", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("phone client: %v", err)
	}

	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("phone listen: %v", err)
	}

	p := &udpPhone{
		ua:      ua,
		cli:     cli,
		addr:    l.LocalAddr().String(),
		target:  "sip:" + engineAddr + ";transport=udp",
		invites: make(chan *sip.Request, 4),
	}

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		p.invites <- req.Clone()
		_ = tx.Respond(sip.NewResponseFromRequest(req, 180, "Ringing", nil))
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", []byte(testSDP2)))
	})

	go srv.ServeUDP(l) //nolint:errcheck
	t.Cleanup(func() { l.Close() })

	return p
}

func (p *udpPhone) register(t *testing.T, ctx context.Context) {
	t.Helper()
	var uri sip.Uri
	if err := sip.ParseUri(p.target, &uri); err != nil {
		t.Fatalf("phone parse target: %v", err)
	}
	req := sip.NewRequest(sip.REGISTER, uri)
	req.AppendHeader(sip.NewHeader("Contact", "<sip:1002@"+p.addr+">;expires=3600"))

	tx, err := p.cli.TransactionRequest(ctx, req)
	if err != nil {
		t.Fatalf("phone REGISTER: %v", err)
	}
	defer tx.Terminate()
	for {
		select {
		case res, ok := <-tx.Responses():
			if !ok {
				t.Fatal("phone REGISTER: transaction closed without final")
			}
			if res.StatusCode >= 200 {
				if res.StatusCode != 200 {
					t.Fatalf("phone REGISTER: expected 200, got %d", res.StatusCode)
				}
				return
			}
		case <-tx.Done():
			t.Fatal("phone REGISTER: done without final")
		case <-ctx.Done():
			t.Fatalf("phone REGISTER: %v", ctx.Err())
		}
	}
}

func (p *udpPhone) waitInvite(t *testing.T, timeout time.Duration) *sip.Request {
	t.Helper()
	select {
	case req := <-p.invites:
		return req
	case <-time.After(timeout):
		t.Fatal("phone: timeout waiting for inbound INVITE")
		return nil
	}
}

// A plain-UDP registered phone (e.g. Twinkle) must be reachable the same way a ws
// webphone is: an inbound INVITE whose top Route is the recorded Path is forwarded
// back to the phone over its recorded flow address, this time over UDP, and the phone
// rings. The existing flow tests only cover the ws transport (connection reuse); this
// covers the udp transport, where the sequencer must reach the phone at flow.Addr.
func TestInboundInviteRoutedBackOverUDPFlow(t *testing.T) {
	registrar := newFakeRegistrar(t)

	plainAddr := freeAddr(t)
	_ = startEngine(t, testConfig(plainAddr, registrar.sipURI(), registrar.sipURI()), 0)

	phone := newUDPPhone(t, plainAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	phone.register(t, ctx)

	reg := registrar.waitRegister(t, 3*time.Second)
	pathValue := pathOf(t, reg)

	res, err := registrar.sendInviteWithRoute(ctx, "sip:"+plainAddr, pathValue)
	if err != nil {
		t.Fatalf("inbound INVITE: %v", err)
	}
	if res.StatusCode >= 300 {
		t.Fatalf("inbound INVITE got %d %s, want the call routed to the phone", res.StatusCode, res.Reason)
	}

	got := phone.waitInvite(t, 4*time.Second)
	if got.Method != sip.INVITE {
		t.Fatalf("phone received %s, want INVITE", got.Method)
	}
}
