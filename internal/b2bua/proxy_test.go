package b2bua

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// ── fakePBXSimple: records any SIP method and responds with a fixed status ───

type fakePBXSimple struct {
	srv     *sipgo.Server
	addr    string
	methods chan sip.RequestMethod
	status  int
	reason  string
}

func newFakePBXSimple(t *testing.T, status int, reason string) *fakePBXSimple {
	t.Helper()

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("fakePBXSimple UA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("fakePBXSimple server: %v", err)
	}

	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakePBXSimple listen: %v", err)
	}

	f := &fakePBXSimple{
		srv:     srv,
		addr:    l.LocalAddr().String(),
		methods: make(chan sip.RequestMethod, 16),
		status:  status,
		reason:  reason,
	}

	srv.OnNoRoute(func(req *sip.Request, tx sip.ServerTransaction) {
		f.methods <- req.Method
		res := sip.NewResponseFromRequest(req, f.status, f.reason, nil)
		_ = tx.Respond(res)
	})

	go srv.ServeUDP(l) //nolint:errcheck
	t.Cleanup(func() { l.Close() })

	return f
}

func (f *fakePBXSimple) sipURI() string { return "sip:" + f.addr }

func (f *fakePBXSimple) waitMethod(t *testing.T, timeout time.Duration) sip.RequestMethod {
	t.Helper()
	select {
	case m := <-f.methods:
		return m
	case <-time.After(timeout):
		t.Fatal("fakePBXSimple: timeout waiting for method")
		return ""
	}
}

func (f *fakePBXSimple) noMethod(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case m := <-f.methods:
		t.Fatalf("fakePBXSimple: unexpected method %s", m)
	case <-time.After(window):
	}
}

// ── fakeUACSimple: sends non-dialog SIP requests and reads responses ─────────

type fakeUACSimple struct {
	cli *sipgo.Client
}

func newFakeUACSimple(t *testing.T) *fakeUACSimple {
	t.Helper()

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("fakeUACSimple UA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("fakeUACSimple server: %v", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("fakeUACSimple client: %v", err)
	}

	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeUACSimple listen: %v", err)
	}

	go srv.ServeUDP(l) //nolint:errcheck
	t.Cleanup(func() { l.Close() })

	return &fakeUACSimple{cli: cli}
}

// send sends a non-dialog SIP request and waits for the first final response.
func (f *fakeUACSimple) send(ctx context.Context, method sip.RequestMethod, targetURI string) (*sip.Response, error) {
	var uri sip.Uri
	if err := sip.ParseUri(targetURI, &uri); err != nil {
		return nil, err
	}
	req := sip.NewRequest(method, uri)
	tx, err := f.cli.TransactionRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	defer tx.Terminate()

	for {
		select {
		case res, ok := <-tx.Responses():
			if !ok {
				return nil, errors.New("transaction terminated without final response")
			}
			if res.StatusCode >= 200 {
				return res, nil
			}
		case <-tx.Done():
			return nil, errors.New("transaction done without response")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ── behavior tests ────────────────────────────────────────────────────────────

// Given engine in front of PBX; When UAC sends REGISTER; Then PBX receives REGISTER and UAC gets PBX status (AC1).
func TestRegisterForwardedToPBX(t *testing.T) {
	pbx := newFakePBXSimple(t, 200, "OK")
	app := newFakeUAS(t)
	uac := newFakeUACSimple(t)

	engAddr := freeAddr(t)
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), pbx.sipURI()), 0)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := uac.send(ctx, sip.REGISTER, "sip:"+engAddr)
	if err != nil {
		t.Fatalf("send REGISTER: %v", err)
	}

	got := pbx.waitMethod(t, time.Second)
	if got != sip.REGISTER {
		t.Fatalf("PBX: expected REGISTER, got %s", got)
	}
	if res.StatusCode != 200 {
		t.Fatalf("UAC: expected 200, got %d", res.StatusCode)
	}
}

// Given engine in front of PBX; When UAC sends OPTIONS; Then PBX receives OPTIONS and UAC gets PBX response (not 405 from engine) (AC2).
func TestOptionsForwardedNotAnsweredLocally(t *testing.T) {
	pbx := newFakePBXSimple(t, 200, "OK")
	app := newFakeUAS(t)
	uac := newFakeUACSimple(t)

	engAddr := freeAddr(t)
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), pbx.sipURI()), 0)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := uac.send(ctx, sip.OPTIONS, "sip:"+engAddr)
	if err != nil {
		t.Fatalf("send OPTIONS: %v", err)
	}

	got := pbx.waitMethod(t, time.Second)
	if got != sip.OPTIONS {
		t.Fatalf("PBX: expected OPTIONS, got %s", got)
	}
	if res.StatusCode == 405 {
		t.Fatal("OPTIONS answered locally with 405; expected forwarding to PBX")
	}
	if res.StatusCode != 200 {
		t.Fatalf("UAC: expected 200, got %d", res.StatusCode)
	}
}

// Given engine in front of PBX; When UAC sends presence/messaging methods; Then PBX receives them (AC3).
func TestPresenceAndMessagingForwarded(t *testing.T) {
	pbx := newFakePBXSimple(t, 200, "OK")
	app := newFakeUAS(t)
	uac := newFakeUACSimple(t)

	engAddr := freeAddr(t)
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), pbx.sipURI()), 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, method := range []sip.RequestMethod{sip.SUBSCRIBE, sip.MESSAGE, sip.PUBLISH} {
		res, err := uac.send(ctx, method, "sip:"+engAddr)
		if err != nil {
			t.Fatalf("send %s: %v", method, err)
		}
		got := pbx.waitMethod(t, time.Second)
		if got != method {
			t.Fatalf("PBX: expected %s, got %s", method, got)
		}
		if res.StatusCode != 200 {
			t.Fatalf("%s: expected 200, got %d", method, res.StatusCode)
		}
	}
}

// Given unmanaged method sent to engine; When forwarded to PBX; Then app chain receives no INVITE (AC4).
func TestUnmanagedMethodsBypassApplicationChain(t *testing.T) {
	pbx := newFakePBXSimple(t, 200, "OK")
	app := newFakeUAS(t)
	uac := newFakeUACSimple(t)

	engAddr := freeAddr(t)
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), pbx.sipURI()), 0)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := uac.send(ctx, sip.REGISTER, "sip:"+engAddr); err != nil {
		t.Fatalf("send REGISTER: %v", err)
	}
	pbx.waitMethod(t, time.Second) // ensure proxy cycle completed

	// Application chain (invoked only from OnInvite → bridge) received nothing.
	app.noInvite(t, 100*time.Millisecond)
}

// Given engine with proxy enabled; When UAC sends INVITE; Then INVITE goes through B2BUA call path (AC5).
func TestInviteStillB2BUAHandled(t *testing.T) {
	pbx := newFakeUAS(t)
	app := newFakeUAS(t)
	caller := newFakeUAC(t)

	engAddr := freeAddr(t)
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), pbx.sipURI()), 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := caller.invite(ctx, "sip:"+engAddr, []byte(testSDP))
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
		t.Fatalf("expected 200 from B2BUA call, got %d", sess.InviteResponse.StatusCode)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
}

// Given PBX responds with non-200; When engine relays it; Then originator receives that exact status (AC6).
func TestForwardedResponseRoutedToSender(t *testing.T) {
	pbx := newFakePBXSimple(t, 403, "Forbidden")
	app := newFakeUAS(t)
	uac := newFakeUACSimple(t)

	engAddr := freeAddr(t)
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), pbx.sipURI()), 0)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := uac.send(ctx, sip.OPTIONS, "sip:"+engAddr)
	if err != nil {
		t.Fatalf("send OPTIONS: %v", err)
	}

	pbx.waitMethod(t, time.Second)

	if res.StatusCode != 403 {
		t.Fatalf("expected 403 relayed from PBX, got %d", res.StatusCode)
	}
}

// Given request with Max-Forwards: 0; When engine receives it; Then 483 Too Many Hops returned without forwarding.
func TestMaxForwardsZeroRejected(t *testing.T) {
	pbx := newFakePBXSimple(t, 200, "OK")
	app := newFakeUAS(t)
	uac := newFakeUACSimple(t)

	engAddr := freeAddr(t)
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), pbx.sipURI()), 0)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// Build OPTIONS with Max-Forwards: 0 manually.
	var uri sip.Uri
	if err := sip.ParseUri("sip:"+engAddr, &uri); err != nil {
		t.Fatalf("parse engine URI: %v", err)
	}
	req := sip.NewRequest(sip.OPTIONS, uri)
	mf := sip.MaxForwardsHeader(0)
	req.AppendHeader(&mf)

	tx, err := uac.cli.TransactionRequest(ctx, req)
	if err != nil {
		t.Fatalf("transaction request: %v", err)
	}
	defer tx.Terminate()

	var res *sip.Response
	select {
	case res = <-tx.Responses():
	case <-tx.Done():
		t.Fatal("transaction done without response")
	case <-ctx.Done():
		t.Fatal("timeout waiting for 483")
	}

	if res.StatusCode != 483 {
		t.Fatalf("expected 483, got %d", res.StatusCode)
	}

	// PBX must NOT have received the request.
	pbx.noMethod(t, 100*time.Millisecond)
}

// ── log-capture helper ────────────────────────────────────────────────────────

type recordHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *recordHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *recordHandler) find(msg string) (slog.Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message == msg {
			return r, true
		}
	}
	return slog.Record{}, false
}

func (h *recordHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Message == msg {
			n++
		}
	}
	return n
}

func installLogCapture(t *testing.T) *recordHandler {
	t.Helper()
	h := &recordHandler{}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })
	return h
}

func recAttrStr(r slog.Record, key string) (string, bool) {
	var val string
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val = a.Value.String()
			found = true
			return false
		}
		return true
	})
	return val, found
}

func recAttrInt(r slog.Record, key string) (int64, bool) {
	var val int64
	var found bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			val = a.Value.Int64()
			found = true
			return false
		}
		return true
	})
	return val, found
}

// ── logging behavior tests ────────────────────────────────────────────────────

// Given engine in front of PBX returning 200; When UAC sends OPTIONS; Then exactly one INFO "proxy forwarded" with method=OPTIONS, status=200, nextHop=PBX.
func TestProxyLogsForwardedOutcome(t *testing.T) {
	pbx := newFakePBXSimple(t, 200, "OK")
	app := newFakeUAS(t)
	uac := newFakeUACSimple(t)

	engAddr := freeAddr(t)
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), pbx.sipURI()), 0)

	h := installLogCapture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := uac.send(ctx, sip.OPTIONS, "sip:"+engAddr)
	if err != nil {
		t.Fatalf("send OPTIONS: %v", err)
	}
	pbx.waitMethod(t, time.Second)

	if n := h.count("proxy forwarded"); n != 1 {
		t.Fatalf("expected 1 'proxy forwarded' record, got %d", n)
	}
	rec, _ := h.find("proxy forwarded")
	if rec.Level != slog.LevelInfo {
		t.Fatalf("expected INFO level, got %v", rec.Level)
	}
	if m, ok := recAttrStr(rec, "method"); !ok || m != string(sip.OPTIONS) {
		t.Fatalf("expected method=OPTIONS, got %q ok=%v", m, ok)
	}
	if s, ok := recAttrInt(rec, "status"); !ok || s != 200 {
		t.Fatalf("expected status=200, got %d ok=%v", s, ok)
	}
	if nh, ok := recAttrStr(rec, "nextHop"); !ok || nh != pbx.addr {
		t.Fatalf("expected nextHop=%s, got %q ok=%v", pbx.addr, nh, ok)
	}
}

// Given PBX returns 403; When engine relays it; Then forwarded line carries status=403.
func TestProxyLogsRelayedNon2xx(t *testing.T) {
	pbx := newFakePBXSimple(t, 403, "Forbidden")
	app := newFakeUAS(t)
	uac := newFakeUACSimple(t)

	engAddr := freeAddr(t)
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), pbx.sipURI()), 0)

	h := installLogCapture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := uac.send(ctx, sip.OPTIONS, "sip:"+engAddr)
	if err != nil {
		t.Fatalf("send OPTIONS: %v", err)
	}

	if n := h.count("proxy forwarded"); n != 1 {
		t.Fatalf("expected 1 'proxy forwarded' record, got %d", n)
	}
	rec, _ := h.find("proxy forwarded")
	if s, ok := recAttrInt(rec, "status"); !ok || s != 403 {
		t.Fatalf("expected status=403, got %d ok=%v", s, ok)
	}
}

// Given Max-Forwards:0; When engine receives request; Then one INFO rejection line and no forwarded line.
func TestProxyLogsMaxForwardsRejection(t *testing.T) {
	pbx := newFakePBXSimple(t, 200, "OK")
	app := newFakeUAS(t)
	uac := newFakeUACSimple(t)

	engAddr := freeAddr(t)
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), pbx.sipURI()), 0)

	h := installLogCapture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var uri sip.Uri
	if err := sip.ParseUri("sip:"+engAddr, &uri); err != nil {
		t.Fatalf("parse engine URI: %v", err)
	}
	req := sip.NewRequest(sip.OPTIONS, uri)
	mf := sip.MaxForwardsHeader(0)
	req.AppendHeader(&mf)

	txClient, err := uac.cli.TransactionRequest(ctx, req)
	if err != nil {
		t.Fatalf("transaction request: %v", err)
	}
	defer txClient.Terminate()

	select {
	case res := <-txClient.Responses():
		if res.StatusCode != 483 {
			t.Fatalf("expected 483, got %d", res.StatusCode)
		}
	case <-txClient.Done():
		t.Fatal("transaction done without response")
	case <-ctx.Done():
		t.Fatal("timeout waiting for 483")
	}

	if n := h.count("proxy rejected: max-forwards exhausted"); n != 1 {
		t.Fatalf("expected 1 rejection record, got %d", n)
	}
	rec, _ := h.find("proxy rejected: max-forwards exhausted")
	if rec.Level != slog.LevelInfo {
		t.Fatalf("expected INFO level, got %v", rec.Level)
	}
	if h.count("proxy forwarded") != 0 {
		t.Fatal("expected no 'proxy forwarded' record for max-forwards rejection")
	}
}

// Given PBX sends 100 then 200; When engine relays; Then only one forwarded line with status=200.
func TestProxyDoesNotLogProvisionals(t *testing.T) {
	pbxUA, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("pbx UA: %v", err)
	}
	pbxSrv, err := sipgo.NewServer(pbxUA)
	if err != nil {
		t.Fatalf("pbx server: %v", err)
	}
	pbxL, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pbx listen: %v", err)
	}
	pbxAddr := pbxL.LocalAddr().String()
	pbxSrv.OnNoRoute(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 100, "Trying", nil))
		time.Sleep(20 * time.Millisecond)
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})
	go pbxSrv.ServeUDP(pbxL) //nolint:errcheck
	t.Cleanup(func() { pbxL.Close() })

	app := newFakeUAS(t)
	uac := newFakeUACSimple(t)

	engAddr := freeAddr(t)
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), "sip:"+pbxAddr), 0)

	h := installLogCapture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err = uac.send(ctx, sip.OPTIONS, "sip:"+engAddr)
	if err != nil {
		t.Fatalf("send OPTIONS: %v", err)
	}

	if n := h.count("proxy forwarded"); n != 1 {
		t.Fatalf("expected exactly 1 'proxy forwarded' record, got %d", n)
	}
	rec, _ := h.find("proxy forwarded")
	if s, ok := recAttrInt(rec, "status"); !ok || s != 200 {
		t.Fatalf("expected status=200, got %d ok=%v", s, ok)
	}
}

// Given unparseable next-hop; When unmanaged request arrives; Then clean 5xx returned and engine stays up.
func TestUnroutableNextHopFailsCleanly(t *testing.T) {
	app := newFakeUAS(t)
	uac := newFakeUACSimple(t)

	engAddr := freeAddr(t)
	// "not-a-sip-uri" has no scheme separator → sip.ParseUri fails → 500 immediately.
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), "not-a-sip-uri"), 0)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := uac.send(ctx, sip.REGISTER, "sip:"+engAddr)
	if err != nil {
		t.Fatalf("send REGISTER: %v", err)
	}
	if res.StatusCode < 500 || res.StatusCode > 599 {
		t.Fatalf("expected 5xx for unparseable next-hop, got %d", res.StatusCode)
	}

	// Engine still responsive: second request also yields 5xx (not crashed).
	res2, err := uac.send(ctx, sip.OPTIONS, "sip:"+engAddr)
	if err != nil {
		t.Fatalf("second send: %v", err)
	}
	if res2.StatusCode < 500 || res2.StatusCode > 599 {
		t.Fatalf("expected 5xx on second request, got %d", res2.StatusCode)
	}
}
