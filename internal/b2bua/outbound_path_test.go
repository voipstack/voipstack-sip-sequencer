package b2bua

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// ── fakeRegistrar: an in-process upstream registrar acting as next_hop ─────────
//
// It captures every REGISTER it receives (so a test can inspect the inserted Path)
// and can originate an inbound INVITE toward the engine carrying a Route header — the
// arriving leg the sequencer must route back over the webphone's flow.

type fakeRegistrar struct {
	cli       *sipgo.Client
	addr      string
	registers chan *sip.Request
}

func newFakeRegistrar(t *testing.T) *fakeRegistrar {
	t.Helper()

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("registrar UA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("registrar server: %v", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("registrar client: %v", err)
	}

	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("registrar listen: %v", err)
	}

	r := &fakeRegistrar{cli: cli, addr: l.LocalAddr().String(), registers: make(chan *sip.Request, 8)}

	srv.OnNoRoute(func(req *sip.Request, tx sip.ServerTransaction) {
		if req.Method == sip.REGISTER {
			r.registers <- req.Clone()
		}
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	go srv.ServeUDP(l) //nolint:errcheck
	t.Cleanup(func() { l.Close() })

	return r
}

func (r *fakeRegistrar) sipURI() string { return "sip:" + r.addr }

func (r *fakeRegistrar) waitRegister(t *testing.T, timeout time.Duration) *sip.Request {
	t.Helper()
	select {
	case req := <-r.registers:
		return req
	case <-time.After(timeout):
		t.Fatal("registrar: timeout waiting for REGISTER")
		return nil
	}
}

// sendInviteWithRoute originates an INVITE to engineURI carrying the given Route
// header value (an addr-spec in angle brackets). It returns the first final response.
func (r *fakeRegistrar) sendInviteWithRoute(ctx context.Context, engineURI, routeValue string) (*sip.Response, error) {
	var uri sip.Uri
	if err := sip.ParseUri(engineURI, &uri); err != nil {
		return nil, err
	}
	req := sip.NewRequest(sip.INVITE, uri)
	req.AppendHeader(sip.NewHeader("Route", routeValue))
	req.AppendHeader(sip.NewHeader("Contact", "<"+r.sipURI()+">"))
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody([]byte(testSDP))

	tx, err := r.cli.TransactionRequest(ctx, req)
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

// ── wsPhone: a real WebSocket SIP client (browser webphone stand-in) ───────────
//
// It dials the engine's ws listener to REGISTER and keeps that one connection open.
// Its UA also serves inbound INVITEs, so an INVITE the engine routes back over the
// flow rings here. The phone runs no listener of its own — it accepts no inbound
// connections — so a ring proves connection reuse, not a fresh dial.

type wsPhone struct {
	ua      *sipgo.UserAgent
	cli     *sipgo.Client
	target  string
	invites chan *sip.Request
}

func newWSPhone(t *testing.T, engineWSAddr string) *wsPhone {
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

	p := &wsPhone{
		ua:      ua,
		cli:     cli,
		target:  "sip:" + engineWSAddr + ";transport=ws",
		invites: make(chan *sip.Request, 4),
	}

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		p.invites <- req.Clone()
		_ = tx.Respond(sip.NewResponseFromRequest(req, 180, "Ringing", nil))
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", []byte(testSDP2)))
	})
	srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {})
	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	t.Cleanup(func() { _ = ua.Close() })
	return p
}

// register sends a REGISTER over the ws flow with Outbound markers and waits for the
// final response.
func (p *wsPhone) register(t *testing.T, ctx context.Context) {
	t.Helper()
	var uri sip.Uri
	if err := sip.ParseUri(p.target, &uri); err != nil {
		t.Fatalf("phone parse target: %v", err)
	}
	req := sip.NewRequest(sip.REGISTER, uri)
	req.AppendHeader(sip.NewHeader("Contact",
		`<sip:phone@example.invalid;transport=ws;ob>;reg-id=1;+sip.instance="<urn:uuid:00000000-0000-0000-0000-000000000001>"`))

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

func (p *wsPhone) waitInvite(t *testing.T, timeout time.Duration) *sip.Request {
	t.Helper()
	select {
	case req := <-p.invites:
		return req
	case <-time.After(timeout):
		t.Fatal("phone: timeout waiting for inbound INVITE")
		return nil
	}
}

func (p *wsPhone) noInvite(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case <-p.invites:
		t.Fatal("phone: unexpected inbound INVITE")
	case <-time.After(window):
	}
}

// ── helpers ────────────────────────────────────────────────────────────────────

// pathOf returns the Path header value of a captured request, failing if absent.
func pathOf(t *testing.T, req *sip.Request) string {
	t.Helper()
	h := req.GetHeader("Path")
	if h == nil {
		t.Fatal("captured REGISTER has no Path header")
	}
	return h.Value()
}

// angleURI parses the addr-spec inside an angle-bracketed header value.
func angleURI(t *testing.T, val string) sip.Uri {
	t.Helper()
	v := strings.TrimSpace(val)
	v = strings.TrimPrefix(v, "<")
	if i := strings.IndexByte(v, '>'); i >= 0 {
		v = v[:i]
	}
	var u sip.Uri
	if err := sip.ParseUri(v, &u); err != nil {
		t.Fatalf("parse angle uri %q: %v", val, err)
	}
	return u
}

// ── behavior tests ───────────────────────────────────────────────────────────

// AC1: a ws client registers; the upstream registrar receives the REGISTER carrying a
// Path whose host is the sequencer and whose token decodes to the client's flow, with
// the Outbound markers carried unchanged.
func TestRegisterInsertsPathAndForwards(t *testing.T) {
	app := newFakeUASTCP(t)
	registrar := newFakeRegistrar(t)

	plainAddr := freeAddr(t)
	wsAddr := freeAddr(t)
	eng := startEngineWS(t, wsConfig(plainAddr, wsAddr, app.sipURI(), registrar.sipURI()))

	phone := newWSPhone(t, wsAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	phone.register(t, ctx)

	reg := registrar.waitRegister(t, 3*time.Second)

	pathURI := angleURI(t, pathOf(t, reg))
	if pathURI.Host != eng.pathHost || pathURI.Port != eng.pathPort {
		t.Fatalf("Path host = %s:%d, want %s:%d", pathURI.Host, pathURI.Port, eng.pathHost, eng.pathPort)
	}
	flow, err := parseFlowToken(pathURI.User, eng.flowSecret)
	if err != nil {
		t.Fatalf("Path token does not verify: %v", err)
	}
	if !strings.EqualFold(flow.Transport, "ws") {
		t.Fatalf("flow transport = %q, want ws", flow.Transport)
	}
	if flow.Addr == "" {
		t.Fatal("flow addr is empty")
	}

	// Outbound markers are carried unchanged to the registrar.
	contact := reg.GetHeader("Contact")
	if contact == nil {
		t.Fatal("forwarded REGISTER lost its Contact")
	}
	for _, marker := range []string{"reg-id", "+sip.instance", "ob"} {
		if !strings.Contains(contact.Value(), marker) {
			t.Fatalf("forwarded Contact %q missing outbound marker %q", contact.Value(), marker)
		}
	}
}

// AC2/NFE: an inbound INVITE whose top Route is the recorded Path is routed back over
// the webphone's existing ws flow — the phone rings without any new connection dialed.
func TestInboundInviteRoutedBackOverFlow(t *testing.T) {
	app := newFakeUASTCP(t)
	registrar := newFakeRegistrar(t)

	plainAddr := freeAddr(t)
	wsAddr := freeAddr(t)
	_ = startEngineWS(t, wsConfig(plainAddr, wsAddr, app.sipURI(), registrar.sipURI()))

	phone := newWSPhone(t, wsAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	phone.register(t, ctx)

	reg := registrar.waitRegister(t, 3*time.Second)
	pathValue := pathOf(t, reg)

	// The registrar sends an inbound INVITE to the sequencer with Route = the Path.
	go func() {
		_, _ = registrar.sendInviteWithRoute(ctx, "sip:"+plainAddr, pathValue)
	}()

	// The phone rings over its existing flow.
	got := phone.waitInvite(t, 4*time.Second)
	if got.Method != sip.INVITE {
		t.Fatalf("phone received %s, want INVITE", got.Method)
	}
}

// AC3: a second request from the same registered client rides the same ws connection.
// The sequencer observes the same source address for both REGISTERs — i.e. one flow,
// one connection reused — so both Paths decode to the same flow.
func TestSubsequentRequestsReuseFlow(t *testing.T) {
	app := newFakeUASTCP(t)
	registrar := newFakeRegistrar(t)

	plainAddr := freeAddr(t)
	wsAddr := freeAddr(t)
	eng := startEngineWS(t, wsConfig(plainAddr, wsAddr, app.sipURI(), registrar.sipURI()))

	phone := newWSPhone(t, wsAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	phone.register(t, ctx)
	first := angleURI(t, pathOf(t, registrar.waitRegister(t, 3*time.Second)))

	// Second request from the same phone reuses the open ws connection.
	phone.register(t, ctx)
	second := angleURI(t, pathOf(t, registrar.waitRegister(t, 3*time.Second)))

	flow1, err := parseFlowToken(first.User, eng.flowSecret)
	if err != nil {
		t.Fatalf("first Path token: %v", err)
	}
	flow2, err := parseFlowToken(second.User, eng.flowSecret)
	if err != nil {
		t.Fatalf("second Path token: %v", err)
	}
	if flow1.Addr != flow2.Addr {
		t.Fatalf("second request used a new connection: flow addr %q != %q", flow2.Addr, flow1.Addr)
	}
}

// AC5: after the client reconnects (a new ws flow) and re-registers, an inbound INVITE
// via the new Path reaches the new flow; the stale Path no longer routes — a dropped
// flow fails gracefully rather than dialing a new connection.
func TestReRegisterUpdatesFlow(t *testing.T) {
	app := newFakeUASTCP(t)
	registrar := newFakeRegistrar(t)

	plainAddr := freeAddr(t)
	wsAddr := freeAddr(t)
	_ = startEngineWS(t, wsConfig(plainAddr, wsAddr, app.sipURI(), registrar.sipURI()))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First flow registers, then goes away (UA closed at cleanup is too late; close now).
	phone1 := newWSPhone(t, wsAddr)
	phone1.register(t, ctx)
	stalePath := pathOf(t, registrar.waitRegister(t, 3*time.Second))
	_ = phone1.ua.Close()

	// A fresh connection re-registers, yielding a new Path.
	phone2 := newWSPhone(t, wsAddr)
	phone2.register(t, ctx)
	freshPath := pathOf(t, registrar.waitRegister(t, 3*time.Second))

	// An INVITE via the fresh Path reaches the new flow.
	go func() { _, _ = registrar.sendInviteWithRoute(ctx, "sip:"+plainAddr, freshPath) }()
	got := phone2.waitInvite(t, 4*time.Second)
	if got.Method != sip.INVITE {
		t.Fatalf("phone2 received %s, want INVITE", got.Method)
	}

	// An INVITE via the stale Path does not reach phone2 (and the gone flow yields a
	// final response rather than a hang).
	res, err := registrar.sendInviteWithRoute(ctx, "sip:"+plainAddr, stalePath)
	if err != nil {
		t.Fatalf("stale-path INVITE: %v", err)
	}
	if res.StatusCode < 400 {
		t.Fatalf("stale-path INVITE: expected a failure status, got %d", res.StatusCode)
	}
	phone2.noInvite(t, 300*time.Millisecond)
}

// Security: an INVITE with a self-host Route whose token fails the MAC is never
// forwarded to the address the token would imply (no SSRF); the request falls through
// to normal handling and a final response is returned.
func TestForgedRouteNotForwarded(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	sentinel := newFakeUAS(t)
	caller := newFakeUAC(t)

	plainAddr := freeAddr(t)
	wsAddr := freeAddr(t)
	eng := startEngineWS(t, wsConfig(plainAddr, wsAddr, app.sipURI(), pbx.sipURI()))

	// Normal bridge can complete so the forged-route INVITE gets a clean final response.
	autoAnswer(t, app, "", nil)
	autoAnswer(t, pbx, "", nil)

	// A token that decodes (only if the MAC were ignored) to the sentinel address, but
	// signed with the WRONG secret — so the sequencer must reject it.
	forged := mintFlowToken(Flow{Addr: sentinel.addr, Transport: "udp"}, []byte("attacker-secret"))
	route := "<sip:" + forged + "@" + eng.pathHost + ":" + strconv.Itoa(eng.pathPort) + ";lr>"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := caller.dcc.Invite(ctx, mustURI(t, "sip:"+plainAddr), []byte(testSDP),
		sip.NewHeader("Route", route))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	_ = sess.Ack(ctx)

	// The sentinel address recovered from the forged token must never be contacted.
	sentinel.noInvite(t, 300*time.Millisecond)
}

func mustURI(t *testing.T, s string) sip.Uri {
	t.Helper()
	var u sip.Uri
	if err := sip.ParseUri(s, &u); err != nil {
		t.Fatalf("parse uri %q: %v", s, err)
	}
	return u
}
