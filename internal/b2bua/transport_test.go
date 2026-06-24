package b2bua

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// newFakeUASTCP is newFakeUAS over a TCP listener only — it never serves UDP. A
// request reaches it only if the sender chose TCP; a UDP datagram sent to this
// TCP port is silently dropped.
func newFakeUASTCP(t *testing.T) *fakeUAS {
	t.Helper()

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("fakeUASTCP UA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("fakeUASTCP server: %v", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("fakeUASTCP client: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeUASTCP listen: %v", err)
	}
	addr := l.Addr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	contact := sip.ContactHeader{Address: sip.Uri{Host: host, Port: port}}
	dsc := sipgo.NewDialogServerCache(cli, contact)

	f := &fakeUAS{srv: srv, dsc: dsc, addr: addr, invites: make(chan *sipgo.DialogServerSession, 8)}

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		dss, err := dsc.ReadInvite(req, tx)
		if err != nil {
			return
		}
		f.invites <- dss
		stateCh := dss.StateRead()
		for {
			select {
			case s := <-stateCh:
				if s >= sip.DialogStateConfirmed {
					return
				}
			case <-tx.Done():
				return
			}
		}
	})
	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = dsc.ReadAck(req, tx)
	})
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = dsc.ReadBye(req, tx)
	})

	go srv.ServeTCP(l) //nolint:errcheck
	t.Cleanup(func() { l.Close() })

	return f
}

// Given an application server reachable only over TCP; When a caller drives the
// chain; Then the engine delivers the app INVITE over TCP (its app URI carries no
// transport param — the engine forces TCP) and the call connects end-to-end.
func TestAppInviteUsesTCP(t *testing.T) {
	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	eng := startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	// The TCP-only app receives the INVITE: proves the engine sent it over TCP.
	go func() {
		dss := app.waitInvite(t, 3*time.Second)
		_ = dss.Respond(180, "Ringing", nil)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()

	// PBX (next-hop) stays on UDP, unchanged.
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", sess.InviteResponse.StatusCode)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call, got %d", n)
	}
}

// A client that registers over TCP must reach the engine. The engine co-binds a plain
// TCP listener on sip.listen alongside UDP (RFC 3261 — a UA/proxy listens on both), so
// the REGISTER connects over TCP and is forwarded to the registrar. Without the TCP
// listener the dial is refused and the registration is impossible over TCP.
func TestRegisterOverTCP(t *testing.T) {
	registrar := newFakeRegistrar(t)

	plainAddr := freeAddr(t)
	_ = startEngine(t, testConfig(plainAddr, registrar.sipURI(), registrar.sipURI()), 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("client UA: %v", err)
	}
	t.Cleanup(func() { _ = ua.Close() })
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	var uri sip.Uri
	if err := sip.ParseUri("sip:"+plainAddr+";transport=tcp", &uri); err != nil {
		t.Fatalf("parse uri: %v", err)
	}
	req := sip.NewRequest(sip.REGISTER, uri)
	req.AppendHeader(sip.NewHeader("Contact", "<sip:1003@example.invalid;transport=tcp>"))

	// Over TCP the connection is opened on send; with no TCP listener this dial is
	// refused and TransactionRequest errors (the reproduction of the bug).
	regTx, err := cli.TransactionRequest(ctx, req)
	if err != nil {
		t.Fatalf("REGISTER over tcp: %v", err)
	}
	defer regTx.Terminate()

	// The engine accepted the TCP connection and forwarded the REGISTER onward.
	reg := registrar.waitRegister(t, 3*time.Second)
	if reg.Method != sip.REGISTER {
		t.Fatalf("registrar received %s, want REGISTER", reg.Method)
	}
}
