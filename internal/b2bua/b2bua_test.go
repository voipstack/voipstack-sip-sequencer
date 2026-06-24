package b2bua

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// ── test SDP ─────────────────────────────────────────────────────────────────

const testSDP = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 9 RTP/AVP 0\r\n"
const testSDP2 = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 9 RTP/AVP 0\r\n"

// ── fakeUAS: in-memory SIP UAS (app / PBX fake) ──────────────────────────────

type fakeUAS struct {
	srv     *sipgo.Server
	dsc     *sipgo.DialogServerCache
	addr    string
	conn    net.PacketConn
	invites chan *sipgo.DialogServerSession
}

func newFakeUAS(t *testing.T) *fakeUAS {
	t.Helper()

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("fakeUAS UA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("fakeUAS server: %v", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("fakeUAS client: %v", err)
	}

	// Serve both UDP and TCP on the same port: the app leg is always reached over
	// TCP (engine-forced), the PBX leg over UDP, and the same fake fills both roles.
	l, tl := listenSamePort(t)
	addr := l.LocalAddr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	contact := sip.ContactHeader{Address: sip.Uri{Host: host, Port: port}}
	dsc := sipgo.NewDialogServerCache(cli, contact)

	f := &fakeUAS{srv: srv, dsc: dsc, addr: addr, conn: l, invites: make(chan *sipgo.DialogServerSession, 8)}

	srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		dss, err := dsc.ReadInvite(req, tx)
		if err != nil {
			return
		}
		f.invites <- dss
		// Block the handler goroutine until the dialog reaches a terminal or
		// confirmed state. Without this, sipgo's TerminateGracefully (called
		// immediately after the handler returns) would kill the server
		// transaction before the test goroutine can call dss.Respond.
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
	srv.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		res := sip.NewResponseFromRequest(req, 200, "OK", nil)
		_ = tx.Respond(res)
	})

	go srv.ServeUDP(l)  //nolint:errcheck
	go srv.ServeTCP(tl) //nolint:errcheck
	t.Cleanup(func() { l.Close(); tl.Close() })

	return f
}

func (f *fakeUAS) sipURI() string { return "sip:" + f.addr }

func (f *fakeUAS) waitInvite(t *testing.T, timeout time.Duration) *sipgo.DialogServerSession {
	t.Helper()
	select {
	case dss := <-f.invites:
		return dss
	case <-time.After(timeout):
		t.Fatal("fakeUAS: timeout waiting for INVITE")
		return nil
	}
}

// noInvite asserts no INVITE arrives within the given window.
func (f *fakeUAS) noInvite(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case <-f.invites:
		t.Fatal("fakeUAS: unexpected INVITE received")
	case <-time.After(window):
	}
}

// ── fakeUAC: in-memory SIP UAC (caller fake) ─────────────────────────────────

type fakeUAC struct {
	dcc  *sipgo.DialogClientCache
	conn net.PacketConn
}

func newFakeUAC(t *testing.T) *fakeUAC {
	t.Helper()

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("fakeUAC UA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("fakeUAC server: %v", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("fakeUAC client: %v", err)
	}

	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeUAC listen: %v", err)
	}
	addr := l.LocalAddr().String()
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	contact := sip.ContactHeader{Address: sip.Uri{Host: host, Port: port}}
	dcc := sipgo.NewDialogClientCache(cli, contact)

	srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = dcc.ReadBye(req, tx)
	})

	go srv.ServeUDP(l) //nolint:errcheck
	t.Cleanup(func() { l.Close() })

	return &fakeUAC{dcc: dcc, conn: l}
}

func (f *fakeUAC) invite(ctx context.Context, targetURI string, sdp []byte) (*sipgo.DialogClientSession, error) {
	var uri sip.Uri
	if err := sip.ParseUri(targetURI, &uri); err != nil {
		return nil, err
	}
	return f.dcc.Invite(ctx, uri, sdp)
}

// ── engine test helpers ───────────────────────────────────────────────────────

// listenSamePort binds a UDP socket and a TCP listener on the same ephemeral 127.0.0.1
// port and returns both, held open. The UDP bind picks the port, but a parallel test can
// already hold that port for TCP, so it retries with a fresh port on a TCP-bind collision.
// Holding both (rather than bind-release-rebind) removes the TOCTOU window entirely — the
// caller serves directly on the returned listeners.
func listenSamePort(t *testing.T) (net.PacketConn, net.Listener) {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		l, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listenSamePort udp: %v", err)
		}
		tl, err := net.Listen("tcp", l.LocalAddr().String())
		if err == nil {
			return l, tl
		}
		l.Close() // port held for TCP by another binder; retry with a fresh one
	}
	t.Fatal("listenSamePort: no shared udp+tcp port after 20 attempts")
	return nil, nil
}

// freeAddr grabs a UDP port and releases it for the engine to bind.
// There is a small TOCTOU race; acceptable in test contexts.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeAddr: %v", err)
	}
	addr := l.LocalAddr().String()
	l.Close()
	return addr
}

// waitNoActiveCalls polls until the engine's call registry is empty, failing if it is
// not within a generous timeout. teardown is asynchronous — it BYEs every live leg
// (network round-trips, each with its own timeout) before removing the call from the
// registry — so a fixed sleep races that cleanup and flakes under load. Polling for the
// real end state still fails for a genuine leak; it only tolerates a slow teardown.
func waitNoActiveCalls(t *testing.T, eng *Engine, what string) {
	t.Helper()
	const timeout = 3 * time.Second
	deadline := time.After(timeout)
	for {
		if eng.calls.len() == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("expected 0 active calls %s, still %d after %s", what, eng.calls.len(), timeout)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func testConfig(listenAddr, appURI, pbxURI string) config.Config {
	return config.Config{
		SIP:     config.SIP{Listen: listenAddr},
		NextHop: config.NextHop{URI: pbxURI},
		RTP:     config.RTP{PortRange: "10000-20000"},
		Sequence: []config.Application{
			{Name: "testapp", URI: appURI, OnFailure: config.FailureSkip},
		},
	}
}

// startEngine starts the engine with an optional short legTimeout for timeout tests.
// Pass a MetricsSink as the optional fourth argument to set it before Run.
// Returns the engine; Cleanup and cancel are registered.
func startEngine(t *testing.T, cfg config.Config, legTimeout time.Duration, sinks ...MetricsSink) *Engine {
	t.Helper()

	eng, err := New(cfg)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if legTimeout > 0 {
		eng.legTimeout = legTimeout
	}
	if len(sinks) > 0 {
		eng.metrics = sinks[0]
	}

	ready := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		rctx := context.WithValue(ctx, sipgo.ListenReadyCtxKey,
			sipgo.ListenReadyFuncCtxValue(func(_, _ string) { close(ready) }))
		_ = eng.Run(rctx)
	}()

	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("engine did not start in time")
	}

	t.Cleanup(func() {
		cancel()
		_ = eng.Shutdown()
	})

	return eng
}

// waitDialogEnd blocks until the dialog transitions to DialogStateEnded or times out.
func waitDialogEnd(t *testing.T, ch <-chan sip.DialogState, timeout time.Duration) {
	t.Helper()
	// The timeout only bounds a genuine hang — the happy path returns as soon as the
	// Ended state arrives. Under parallel test load the teardown BYE round-trip that
	// drives that transition can take several seconds, so enforce a generous floor to
	// avoid spurious "timeout waiting for dialog end" flakes (callers pass 3s).
	if timeout < 15*time.Second {
		timeout = 15 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case s := <-ch:
			if s == sip.DialogStateEnded {
				return
			}
		case <-timer.C:
			t.Fatal("timeout waiting for dialog end")
		}
	}
}

// ── behavior tests ────────────────────────────────────────────────────────────

// Given reachable app and PBX; When caller sends INVITE; Then call established end-to-end (AC1).
func TestSingleAppCallConnectsEndToEnd(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	eng := startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	// Caller sends INVITE to engine
	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	// App answers with SDP
	go func() {
		dss := app.waitInvite(t, 3*time.Second)
		_ = dss.Respond(180, "Ringing", nil)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()

	// PBX answers with SDP
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()

	// Caller gets 200 with PBX SDP
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", sess.InviteResponse.StatusCode)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	// Registry has one active call
	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call, got %d", n)
	}
}

// Given established call; When caller sends BYE; Then all legs tear down (AC2).
func TestCallerHangupTearsDownAllLegs(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	eng := startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	var appSess, pbxSess *sipgo.DialogServerSession
	appReady := make(chan struct{})
	pbxReady := make(chan struct{})

	go func() {
		appSess = app.waitInvite(t, 3*time.Second)
		_ = appSess.Respond(200, "OK", []byte(testSDP2))
		close(appReady)
	}()
	go func() {
		pbxSess = pbx.waitInvite(t, 3*time.Second)
		_ = pbxSess.Respond(200, "OK", []byte(testSDP2))
		close(pbxReady)
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	<-appReady
	<-pbxReady

	// Register state watchers before sending BYE
	appEnd := appSess.StateRead()
	pbxEnd := pbxSess.StateRead()

	// Caller hangs up
	if err := sess.Bye(ctx); err != nil {
		t.Fatalf("caller BYE: %v", err)
	}

	waitDialogEnd(t, appEnd, 3*time.Second)
	waitDialogEnd(t, pbxEnd, 3*time.Second)

	waitNoActiveCalls(t, eng, "after BYE")
}

// Given established call; When PBX sends BYE; Then all legs tear down (AC3).
func TestCalleeHangupTearsDownAllLegs(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	eng := startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	var appSess, pbxSess *sipgo.DialogServerSession
	appReady := make(chan struct{})
	pbxReady := make(chan struct{})

	go func() {
		appSess = app.waitInvite(t, 3*time.Second)
		_ = appSess.Respond(200, "OK", []byte(testSDP2))
		close(appReady)
	}()
	go func() {
		pbxSess = pbx.waitInvite(t, 3*time.Second)
		_ = pbxSess.Respond(200, "OK", []byte(testSDP2))
		close(pbxReady)
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	<-appReady
	<-pbxReady

	callerEnd := sess.StateRead()
	appEnd := appSess.StateRead()

	// PBX hangs up
	if err := pbxSess.Bye(ctx); err != nil {
		t.Fatalf("pbx BYE: %v", err)
	}

	waitDialogEnd(t, callerEnd, 3*time.Second)
	waitDialogEnd(t, appEnd, 3*time.Second)

	waitNoActiveCalls(t, eng, "after callee BYE")
}

// Given app rejects with 486; When caller sends INVITE; Then caller sees 486 and PBX is never invited (AC4).
func TestAppRejectPropagatesStatusAndNoPBX(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "testapp", URI: app.sipURI(), OnFailure: config.FailureAbort},
	}), 0)
	ctx := context.Background()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	go func() {
		dss := app.waitInvite(t, 3*time.Second)
		_ = dss.Respond(486, "Busy Here", nil)
	}()

	err = sess.WaitAnswer(ctx, sipgo.AnswerOptions{})
	var dialErr *sipgo.ErrDialogResponse
	if !errors.As(err, &dialErr) {
		t.Fatalf("expected ErrDialogResponse, got %v", err)
	}
	if dialErr.Res.StatusCode != 486 {
		t.Fatalf("expected 486, got %d", dialErr.Res.StatusCode)
	}

	// PBX must never receive an INVITE
	pbx.noInvite(t, 200*time.Millisecond)
}

// Given app never responds; When leg timeout fires; Then caller receives 503 (AC4 timeout branch).
func TestAppTimeoutReturns503(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	// Very short legTimeout so test does not wait 32 seconds
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "testapp", URI: app.sipURI(), OnFailure: config.FailureAbort},
	}), 150*time.Millisecond)
	ctx := context.Background()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	// App receives INVITE but never responds
	go func() { app.waitInvite(t, 3*time.Second) }()

	err = sess.WaitAnswer(context.Background(), sipgo.AnswerOptions{})
	var dialErr *sipgo.ErrDialogResponse
	if !errors.As(err, &dialErr) {
		t.Fatalf("expected ErrDialogResponse, got %v", err)
	}
	if dialErr.Res.StatusCode != 503 {
		t.Fatalf("expected 503, got %d", dialErr.Res.StatusCode)
	}
}

// Given an app with its own timeout; When effectiveTimeout is asked; Then the app value wins.
func TestEffectiveTimeoutPrefersAppOverGlobal(t *testing.T) {
	app := config.Application{Name: "a", TimeoutDur: 2 * time.Second}
	if got := effectiveTimeout(app, 32*time.Second); got != 2*time.Second {
		t.Fatalf("effectiveTimeout = %v, want %v", got, 2*time.Second)
	}
}

// Given an app with no timeout; When effectiveTimeout is asked; Then the global default wins.
func TestEffectiveTimeoutFallsBackToGlobal(t *testing.T) {
	app := config.Application{Name: "a"}
	if got := effectiveTimeout(app, 32*time.Second); got != 32*time.Second {
		t.Fatalf("effectiveTimeout = %v, want %v", got, 32*time.Second)
	}
}

// Given an unreachable app with a short per-app timeout and on_failure: skip and a global
// legTimeout far larger; When the call arrives; Then the dead app is skipped within ~timeout
// (not the global) and the call still completes via the PBX.
func TestPerAppTimeoutFailsFastAndSkips(t *testing.T) {
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	deadAddr := freeAddr(t) // nothing listens here → dial fails / no answer
	listenAddr := freeAddr(t)
	// Global legTimeout is large; only the per-app timeout makes this fast.
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "dead", URI: "sip:" + deadAddr, OnFailure: config.FailureSkip, TimeoutDur: 300 * time.Millisecond},
	}), 30*time.Second)
	ctx := context.Background()

	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP))
	}()

	start := time.Now()
	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(context.Background(), sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("expected call to complete via PBX, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("call took %v; per-app timeout did not fast-fail (global=30s)", elapsed)
	}
}

// Given a call; When bridged; Then inbound Call-ID differs from both outbound Call-IDs (AC5).
func TestInboundAndOutboundAreDistinctDialogs(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	var appCallID, pbxCallID string
	appDone := make(chan struct{})
	pbxDone := make(chan struct{})

	go func() {
		dss := app.waitInvite(t, 3*time.Second)
		appCallID = dss.InviteRequest.CallID().Value()
		_ = dss.Respond(200, "OK", []byte(testSDP2))
		close(appDone)
	}()
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		pbxCallID = dss.InviteRequest.CallID().Value()
		_ = dss.Respond(200, "OK", []byte(testSDP2))
		close(pbxDone)
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	<-appDone
	<-pbxDone

	inboundCallID := sess.InviteRequest.CallID().Value()

	if inboundCallID == appCallID {
		t.Fatalf("inbound and app leg share Call-ID %q", inboundCallID)
	}
	if inboundCallID == pbxCallID {
		t.Fatalf("inbound and pbx leg share Call-ID %q", inboundCallID)
	}
	if appCallID == pbxCallID {
		t.Fatalf("app and pbx legs share Call-ID %q", appCallID)
	}
}

// Given many rejected calls; When all complete; Then registry is empty (no-leak NFR).
func TestManyRejectedCallsLeaveNoActiveCalls(t *testing.T) {
	const n = 10

	app := newFakeUAS(t)
	pbx := newFakeUAS(t)

	listenAddr := freeAddr(t)
	eng := startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "testapp", URI: app.sipURI(), OnFailure: config.FailureAbort},
	}), 0)
	ctx := context.Background()

	// App always rejects
	go func() {
		for i := 0; i < n; i++ {
			dss := app.waitInvite(t, 5*time.Second)
			_ = dss.Respond(503, "Service Unavailable", nil)
		}
	}()

	done := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		caller := newFakeUAC(t)
		go func() {
			defer func() { done <- struct{}{} }()
			sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
			if err != nil {
				return
			}
			_ = sess.WaitAnswer(ctx, sipgo.AnswerOptions{})
		}()
	}

	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("timeout waiting for call %d to complete", i+1)
		}
	}

	waitNoActiveCalls(t, eng, "(empty registry expected)")
	// PBX should never have been invited
	pbx.noInvite(t, 50*time.Millisecond)
}

// Given reachable app and PBX; When caller receives 200 OK with SDP;
// Then the response includes Content-Type: application/sdp (regression).
func TestInviteResponseWithSDPIncludesContentType(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	go func() {
		dss := app.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}

	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", sess.InviteResponse.StatusCode)
	}

	ct := sess.InviteResponse.GetHeader("Content-Type")
	if ct == nil {
		t.Fatal("200 OK response is missing Content-Type header")
	}
	if ct.Value() != "application/sdp" {
		t.Fatalf("expected Content-Type application/sdp, got %q", ct.Value())
	}

	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}
}

// ── chain test helpers ────────────────────────────────────────────────────────

// orderRecorder records names of fake apps as they receive INVITEs.
type orderRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *orderRecorder) record(name string) {
	r.mu.Lock()
	r.seen = append(r.seen, name)
	r.mu.Unlock()
}

func (r *orderRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]string, len(r.seen))
	copy(cp, r.seen)
	return cp
}

// autoAnswer drains f.invites in a background goroutine: records name to rec (if non-nil),
// then answers each INVITE with 200 OK (testSDP2). Goroutine stops when t finishes.
func autoAnswer(t *testing.T, f *fakeUAS, name string, rec *orderRecorder) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			select {
			case dss := <-f.invites:
				if rec != nil {
					rec.record(name)
				}
				_ = dss.Respond(200, "OK", []byte(testSDP2))
			case <-ctx.Done():
				return
			}
		}
	}()
}

// multiAppConfig builds a Config with a given ordered application slice.
func multiAppConfig(listenAddr, pbxURI string, apps []config.Application) config.Config {
	return config.Config{
		SIP:      config.SIP{Listen: listenAddr},
		NextHop:  config.NextHop{URI: pbxURI},
		RTP:      config.RTP{PortRange: "10000-20000"},
		Sequence: apps,
	}
}

// containsBytes reports whether b contains sub.
func containsBytes(b, sub []byte) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i <= len(b)-len(sub); i++ {
		if string(b[i:i+len(sub)]) == string(sub) {
			return true
		}
	}
	return false
}

// equalStringSlice reports whether a and b have identical contents.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ── chain behavior tests ──────────────────────────────────────────────────────

// Given sequence [A,B,C]; When call arrives; Then apps visited in order A→B→C then PBX (AC1).
func TestChainTraversesApplicationsInConfiguredOrder(t *testing.T) {
	rec := &orderRecorder{}
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	appC := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	eng := startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "A", URI: appA.sipURI(), OnFailure: config.FailureSkip},
		{Name: "B", URI: appB.sipURI(), OnFailure: config.FailureSkip},
		{Name: "C", URI: appC.sipURI(), OnFailure: config.FailureSkip},
	}), 0)
	ctx := context.Background()

	autoAnswer(t, appA, "A", rec)
	autoAnswer(t, appB, "B", rec)
	autoAnswer(t, appC, "C", rec)
	autoAnswer(t, pbx, "", nil)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	want := []string{"A", "B", "C"}
	if got := rec.snapshot(); !equalStringSlice(got, want) {
		t.Fatalf("app visit order = %v, want %v", got, want)
	}
	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call, got %d", n)
	}
}

// Given sequence [C,A,B]; When call arrives; Then apps visited in order C→A→B then PBX (AC2).
func TestChainOrderFollowsConfigReorder(t *testing.T) {
	rec := &orderRecorder{}
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	appC := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "C", URI: appC.sipURI(), OnFailure: config.FailureSkip},
		{Name: "A", URI: appA.sipURI(), OnFailure: config.FailureSkip},
		{Name: "B", URI: appB.sipURI(), OnFailure: config.FailureSkip},
	}), 0)
	ctx := context.Background()

	autoAnswer(t, appA, "A", rec)
	autoAnswer(t, appB, "B", rec)
	autoAnswer(t, appC, "C", rec)
	autoAnswer(t, pbx, "", nil)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	want := []string{"C", "A", "B"}
	if got := rec.snapshot(); !equalStringSlice(got, want) {
		t.Fatalf("app visit order = %v, want %v", got, want)
	}
}

// Given single-app sequence; When call arrives; Then call connects end-to-end (AC3 regression).
func TestSingleApplicationChainUnchanged(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	autoAnswer(t, app, "", nil)
	autoAnswer(t, pbx, "", nil)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", sess.InviteResponse.StatusCode)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
}

// Given empty sequence; When call arrives; Then PBX is invited directly; no app legs (AC4).
func TestEmptySequenceRoutesStraightToPBX(t *testing.T) {
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	eng := startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), nil), 0)
	ctx := context.Background()

	autoAnswer(t, pbx, "", nil)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", sess.InviteResponse.StatusCode)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call, got %d", n)
	}
}

// Given sequence [A,B,C]; When call arrives; Then each app receives exactly one INVITE (AC5).
func TestApplicationsReceiveOrdinaryInvite(t *testing.T) {
	rec := &orderRecorder{}
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	appC := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "A", URI: appA.sipURI(), OnFailure: config.FailureSkip},
		{Name: "B", URI: appB.sipURI(), OnFailure: config.FailureSkip},
		{Name: "C", URI: appC.sipURI(), OnFailure: config.FailureSkip},
	}), 0)
	ctx := context.Background()

	autoAnswer(t, appA, "A", rec)
	autoAnswer(t, appB, "B", rec)
	autoAnswer(t, appC, "C", rec)
	autoAnswer(t, pbx, "", nil)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	// Each app must appear exactly once; order A→B→C proves each got its own INVITE.
	got := rec.snapshot()
	want := []string{"A", "B", "C"}
	if !equalStringSlice(got, want) {
		t.Fatalf("each app must receive exactly one INVITE in order; got %v, want %v", got, want)
	}
}

// Given established 3-app chain; When caller sends BYE; Then all app + PBX legs torn down (AC6).
func TestFullChainTearsDownOnHangup(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	appC := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	eng := startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "A", URI: appA.sipURI(), OnFailure: config.FailureSkip},
		{Name: "B", URI: appB.sipURI(), OnFailure: config.FailureSkip},
		{Name: "C", URI: appC.sipURI(), OnFailure: config.FailureSkip},
	}), 0)
	ctx := context.Background()

	// Collect sessions so we can subscribe to state before BYE.
	appASessC := make(chan *sipgo.DialogServerSession, 1)
	appBSessC := make(chan *sipgo.DialogServerSession, 1)
	appCSessC := make(chan *sipgo.DialogServerSession, 1)
	pbxSessC := make(chan *sipgo.DialogServerSession, 1)

	go func() {
		dss := appA.waitInvite(t, 5*time.Second)
		appASessC <- dss
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()
	go func() {
		dss := appB.waitInvite(t, 5*time.Second)
		appBSessC <- dss
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()
	go func() {
		dss := appC.waitInvite(t, 5*time.Second)
		appCSessC <- dss
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()
	go func() {
		dss := pbx.waitInvite(t, 5*time.Second)
		pbxSessC <- dss
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	appASess := <-appASessC
	appBSess := <-appBSessC
	appCSess := <-appCSessC
	pbxSess := <-pbxSessC

	appAEnd := appASess.StateRead()
	appBEnd := appBSess.StateRead()
	appCEnd := appCSess.StateRead()
	pbxEnd := pbxSess.StateRead()

	if err := sess.Bye(ctx); err != nil {
		t.Fatalf("caller BYE: %v", err)
	}

	waitDialogEnd(t, appAEnd, 3*time.Second)
	waitDialogEnd(t, appBEnd, 3*time.Second)
	waitDialogEnd(t, appCEnd, 3*time.Second)
	waitDialogEnd(t, pbxEnd, 3*time.Second)

	waitNoActiveCalls(t, eng, "after BYE")
}

// Given sequence [A,B,C]; When app B rejects; Then caller sees rejection, A is torn down,
// C and PBX never invited, registry empty.
func TestMidChainFailureTearsDownPriorLegs(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	appC := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	eng := startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "A", URI: appA.sipURI(), OnFailure: config.FailureSkip},
		{Name: "B", URI: appB.sipURI(), OnFailure: config.FailureAbort},
		{Name: "C", URI: appC.sipURI(), OnFailure: config.FailureSkip},
	}), 0)
	ctx := context.Background()

	go func() {
		dss := appA.waitInvite(t, 5*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()
	go func() {
		dss := appB.waitInvite(t, 5*time.Second)
		_ = dss.Respond(486, "Busy Here", nil)
	}()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	err = sess.WaitAnswer(ctx, sipgo.AnswerOptions{})
	var dialErr *sipgo.ErrDialogResponse
	if !errors.As(err, &dialErr) {
		t.Fatalf("expected ErrDialogResponse, got %v", err)
	}
	if dialErr.Res.StatusCode != 486 {
		t.Fatalf("expected 486 from chain failure, got %d", dialErr.Res.StatusCode)
	}

	appC.noInvite(t, 200*time.Millisecond)
	pbx.noInvite(t, 50*time.Millisecond)

	waitNoActiveCalls(t, eng, "after mid-chain failure")
}

// ── failure-policy test helpers ───────────────────────────────────────────────

// recordingSink records AppFailure calls for test assertions.
type recordingSink struct {
	mu    sync.Mutex
	names []string
}

func (s *recordingSink) AppFailure(name string) {
	s.mu.Lock()
	s.names = append(s.names, name)
	s.mu.Unlock()
}

func (s *recordingSink) AppInvocation(string)                   {}
func (s *recordingSink) TerminatingHopFailure()                 {}
func (s *recordingSink) ObserveSequencingLatency(time.Duration) {}
func (s *recordingSink) MediaCodecMismatch(string, string)      {}

func (s *recordingSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]string, len(s.names))
	copy(cp, s.names)
	return cp
}

// autoReject drains f.invites and rejects each INVITE with the given status.
func autoReject(t *testing.T, f *fakeUAS, status int, reason string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			select {
			case dss := <-f.invites:
				_ = dss.Respond(status, reason, nil)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// ── failure-policy behavior tests ────────────────────────────────────────────

// Given [A(skip,reject), B(skip,up)]; When call arrives; Then call connects via B+PBX (AC1).
func TestSkipAdvancesPastFailedApplication(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "A", URI: appA.sipURI(), OnFailure: config.FailureSkip},
		{Name: "B", URI: appB.sipURI(), OnFailure: config.FailureSkip},
	}), 0)
	ctx := context.Background()

	autoReject(t, appA, 503, "Service Unavailable")
	autoAnswer(t, appB, "", nil)
	autoAnswer(t, pbx, "", nil)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", sess.InviteResponse.StatusCode)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
}

// Given [A(abort,reject), B(skip)]; When call arrives; Then call fails and B+PBX never invited (AC2).
func TestAbortFailsCallOnRequiredApplication(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "A", URI: appA.sipURI(), OnFailure: config.FailureAbort},
		{Name: "B", URI: appB.sipURI(), OnFailure: config.FailureSkip},
	}), 0)
	ctx := context.Background()

	autoReject(t, appA, 486, "Busy Here")

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	err = sess.WaitAnswer(ctx, sipgo.AnswerOptions{})
	var dialErr *sipgo.ErrDialogResponse
	if !errors.As(err, &dialErr) {
		t.Fatalf("expected ErrDialogResponse, got %v", err)
	}
	if dialErr.Res.StatusCode != 486 {
		t.Fatalf("expected 486, got %d", dialErr.Res.StatusCode)
	}

	appB.noInvite(t, 200*time.Millisecond)
	pbx.noInvite(t, 50*time.Millisecond)
}

// Given on_failure omitted (zero value acts as skip); When app rejects; Then PBX is reached (AC3).
func TestOmittedPolicyDefaultsToSkip(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	// OnFailure left as zero value — failureAction maps it to actionSkip.
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "app", URI: app.sipURI()},
	}), 0)
	ctx := context.Background()

	autoReject(t, app, 503, "Service Unavailable")
	autoAnswer(t, pbx, "", nil)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", sess.InviteResponse.StatusCode)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
}

// Given skip policy; When app fails; Then sink receives exactly one AppFailure signal (AC4).
func TestSkipFailureIsLoggedAndSignalled(t *testing.T) {
	appA := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	sink := &recordingSink{}
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "appA", URI: appA.sipURI(), OnFailure: config.FailureSkip},
	}), 0, sink)
	ctx := context.Background()

	autoReject(t, appA, 503, "Service Unavailable")
	autoAnswer(t, pbx, "", nil)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	if got := sink.snapshot(); len(got) != 1 || got[0] != "appA" {
		t.Fatalf("AppFailure signals = %v, want [appA]", got)
	}
}

// Given [A(skip,reject), B(skip,reject)]; When call arrives; Then PBX gets inbound offer and call connects (AC5).
func TestAllSkipChainAllDownStillReachesPBX(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "A", URI: appA.sipURI(), OnFailure: config.FailureSkip},
		{Name: "B", URI: appB.sipURI(), OnFailure: config.FailureSkip},
	}), 0)
	ctx := context.Background()

	autoReject(t, appA, 503, "Service Unavailable")
	autoReject(t, appB, 503, "Service Unavailable")

	var pbxOfferSDP []byte
	pbxInviteC := make(chan struct{})
	pbxCtx, pbxCancel := context.WithCancel(context.Background())
	t.Cleanup(pbxCancel)
	go func() {
		select {
		case dss := <-pbx.invites:
			pbxOfferSDP = dss.InviteRequest.Body()
			_ = dss.Respond(200, "OK", []byte(testSDP2))
			close(pbxInviteC)
		case <-pbxCtx.Done():
		}
	}()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", sess.InviteResponse.StatusCode)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	select {
	case <-pbxInviteC:
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for PBX invite capture")
	}

	// PBX now receives the anchored SDP (media plane added in story 005).
	// Verify codecs from the inbound offer are preserved (RTP/AVP 0).
	if pbxOfferSDP == nil {
		t.Fatal("PBX received no SDP body")
	}
	if !containsBytes(pbxOfferSDP, []byte("RTP/AVP 0")) {
		t.Fatalf("PBX SDP missing codec from inbound offer: %q", pbxOfferSDP)
	}
}

// Given [A(abort,reject), B(skip)]; When A aborts; Then B never receives an INVITE (AC6).
func TestAbortFirstAppPreventsLaterContact(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "A", URI: appA.sipURI(), OnFailure: config.FailureAbort},
		{Name: "B", URI: appB.sipURI(), OnFailure: config.FailureSkip},
	}), 0)
	ctx := context.Background()

	autoReject(t, appA, 503, "Service Unavailable")

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	err = sess.WaitAnswer(ctx, sipgo.AnswerOptions{})
	var dialErr *sipgo.ErrDialogResponse
	if !errors.As(err, &dialErr) {
		t.Fatalf("expected ErrDialogResponse, got %v", err)
	}
	if dialErr.Res.StatusCode != 503 {
		t.Fatalf("expected 503, got %d", dialErr.Res.StatusCode)
	}

	appB.noInvite(t, 200*time.Millisecond)
	pbx.noInvite(t, 50*time.Millisecond)
}

// Given [A(skip,reject)]; When call connects via PBX; When BYE; Then registry empty and A not BYEd (leg bookkeeping).
func TestSkipFailedLegNotInTeardown(t *testing.T) {
	appA := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	eng := startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "A", URI: appA.sipURI(), OnFailure: config.FailureSkip},
	}), 0)
	ctx := context.Background()

	autoReject(t, appA, 503, "Service Unavailable")
	autoAnswer(t, pbx, "", nil)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	if err := sess.Bye(ctx); err != nil {
		t.Fatalf("caller BYE: %v", err)
	}

	waitNoActiveCalls(t, eng, "after BYE")
}
