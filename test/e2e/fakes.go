//go:build e2e

package e2e

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
)

// These fakes mirror the in-process ones in internal/b2bua/b2bua_test.go, which
// are package-private and not importable here. They are real sipgo agents (real
// fakes, per AGENTS.md), not assertion mocks.

const (
	testSDP  = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 9 RTP/AVP 0\r\n"
	testSDP2 = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 9 RTP/AVP 0\r\n"
)

// sdpWithAddr builds a minimal audio SDP advertising the given RTP host and port,
// used when a leg must offer/answer a real UDP socket the test controls.
func sdpWithAddr(host string, port int) []byte {
	return []byte("v=0\r\no=- 0 0 IN IP4 " + host + "\r\ns=-\r\nc=IN IP4 " + host +
		"\r\nt=0 0\r\nm=audio " + strconv.Itoa(port) + " RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
}

// tapAnswerSDP builds a two-stream recvonly answer for a tap-mode app: stream one
// receives the caller direction, stream two the callee direction.
func tapAnswerSDP(host string, port1, port2 int) []byte {
	return []byte("v=0\r\no=- 0 0 IN IP4 " + host + "\r\ns=-\r\nt=0 0\r\n" +
		"m=audio " + strconv.Itoa(port1) + " RTP/AVP 0\r\nc=IN IP4 " + host + "\r\na=rtpmap:0 PCMU/8000\r\na=recvonly\r\n" +
		"m=audio " + strconv.Itoa(port2) + " RTP/AVP 0\r\nc=IN IP4 " + host + "\r\na=rtpmap:0 PCMU/8000\r\na=recvonly\r\n")
}

// ── fakeUAS: SIP UAS serving UDP+TCP on one port (app and PBX roles) ──────────

type fakeUAS struct {
	srv     *sipgo.Server
	dsc     *sipgo.DialogServerCache
	addr    string
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

	// Serve both UDP and TCP on the same port: the app leg is reached over TCP
	// (engine-forced), the PBX leg over UDP, and one fake fills both roles. Binding
	// UDP :0 then TCP on that same port has a race — another socket can grab the TCP
	// port in between — so retry the pairing until both bind on one port.
	l, tl, addr := bindUDPAndTCP(t)
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)

	contact := sip.ContactHeader{Address: sip.Uri{Host: host, Port: port}}
	dsc := sipgo.NewDialogServerCache(cli, contact)

	f := &fakeUAS{srv: srv, dsc: dsc, addr: addr, invites: make(chan *sipgo.DialogServerSession, 8)}
	f.installHandlers(srv, dsc)

	go srv.ServeUDP(l)  //nolint:errcheck
	go srv.ServeTCP(tl) //nolint:errcheck

	return f
}

// installHandlers wires the standard UAS dialog handlers onto srv: surface each
// INVITE on f.invites (blocking the handler until the dialog confirms so sipgo
// does not terminate the transaction before the test answers), and read ACK/BYE
// /CANCEL through dsc.
func (f *fakeUAS) installHandlers(srv *sipgo.Server, dsc *sipgo.DialogServerCache) {
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
	srv.OnCancel(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})
}

// bindUDPAndTCP returns a UDP and a TCP listener bound to the same 127.0.0.1 port,
// retrying with a fresh port when the TCP bind loses the race for the UDP-chosen
// port. Both are registered for cleanup.
func bindUDPAndTCP(t *testing.T) (net.PacketConn, net.Listener, string) {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		l, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("fakeUAS listen udp: %v", err)
		}
		addr := l.LocalAddr().String()
		tl, err := net.Listen("tcp", addr)
		if err != nil {
			l.Close() // TCP port taken; try another
			continue
		}
		t.Cleanup(func() { l.Close(); tl.Close() })
		return l, tl, addr
	}
	t.Fatal("bindUDPAndTCP: no free UDP+TCP port pair after retries")
	return nil, nil, ""
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

// autoAnswer drains every inbound INVITE in the background and answers 200 with
// the given SDP, registering an end-state watcher per dialog. The returned ended
// channel receives once when a dialog reaches DialogStateEnded.
func autoAnswer(t *testing.T, f *fakeUAS, sdp []byte) <-chan struct{} {
	t.Helper()
	ended := make(chan struct{}, 4)
	go func() {
		for {
			select {
			case dss := <-f.invites:
				watchEnd(dss, ended)
				_ = dss.Respond(200, "OK", sdp)
			case <-time.After(10 * time.Second):
				return
			}
		}
	}()
	return ended
}

// ── chain recording ──────────────────────────────────────────────────────────

// chainLog records, in arrival order, the apps that received an INVITE and the
// INVITE request each got — so chain tests can assert ordering and header
// propagation across legs.
type chainLog struct {
	mu    sync.Mutex
	order []string
	reqs  map[string]*sip.Request
}

func newChainLog() *chainLog { return &chainLog{reqs: map[string]*sip.Request{}} }

func (l *chainLog) record(name string, req *sip.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.order = append(l.order, name)
	l.reqs[name] = req
}

func (l *chainLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.order...)
}

func (l *chainLog) request(name string) *sip.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reqs[name]
}

// serveApp drains a fake app's INVITEs in the background, records each into log
// under name, and answers with the given status/SDP. A non-2xx status models an
// app rejection (skip/abort policy). The goroutine stops when t finishes.
func serveApp(t *testing.T, f *fakeUAS, name string, log *chainLog, status int, reason string, sdp []byte) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			select {
			case dss := <-f.invites:
				if log != nil {
					log.record(name, dss.InviteRequest)
				}
				_ = dss.Respond(status, reason, sdp)
			case <-ctx.Done():
				return
			}
		}
	}()
}

// watchEnd signals ended once when the dialog terminates.
func watchEnd(dss *sipgo.DialogServerSession, ended chan<- struct{}) {
	stateCh := dss.StateRead()
	go func() {
		for s := range stateCh {
			if s == sip.DialogStateEnded {
				select {
				case ended <- struct{}{}:
				default:
				}
				return
			}
		}
	}()
}

// ── fakeUAC: SIP UAC caller over UDP ─────────────────────────────────────────

type fakeUAC struct {
	dcc *sipgo.DialogClientCache
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

	return &fakeUAC{dcc: dcc}
}

func (f *fakeUAC) invite(ctx context.Context, targetURI string, sdp []byte) (*sipgo.DialogClientSession, error) {
	var uri sip.Uri
	if err := sip.ParseUri(targetURI, &uri); err != nil {
		return nil, err
	}
	return f.dcc.Invite(ctx, uri, sdp)
}

// ── fakeNextHop: records unmanaged (non-dialog) methods proxied to it ─────────

// fakeNextHop stands in for the terminating PBX for proxy tests: it captures every
// non-dialog method the engine forwards (via OnNoRoute) and answers a fixed status.
type fakeNextHop struct {
	addr    string
	methods chan sip.RequestMethod
	reqs    chan *sip.Request
}

func newFakeNextHop(t *testing.T) *fakeNextHop {
	t.Helper()
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("fakeNextHop UA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("fakeNextHop server: %v", err)
	}
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fakeNextHop listen: %v", err)
	}

	f := &fakeNextHop{
		addr:    l.LocalAddr().String(),
		methods: make(chan sip.RequestMethod, 16),
		reqs:    make(chan *sip.Request, 16),
	}
	srv.OnNoRoute(func(req *sip.Request, tx sip.ServerTransaction) {
		f.methods <- req.Method
		f.reqs <- req
		_ = tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
	})

	go srv.ServeUDP(l) //nolint:errcheck
	t.Cleanup(func() { l.Close() })
	return f
}

func (f *fakeNextHop) sipURI() string { return "sip:" + f.addr }

func (f *fakeNextHop) waitMethod(t *testing.T, timeout time.Duration) sip.RequestMethod {
	t.Helper()
	select {
	case m := <-f.methods:
		return m
	case <-time.After(timeout):
		t.Fatal("fakeNextHop: timeout waiting for a proxied method")
		return ""
	}
}

// waitRequest returns the next request the engine forwarded here, so a test can
// inspect the on-the-wire headers (e.g. the Via list) the next hop actually saw.
func (f *fakeNextHop) waitRequest(t *testing.T, timeout time.Duration) *sip.Request {
	t.Helper()
	select {
	case req := <-f.reqs:
		return req
	case <-time.After(timeout):
		t.Fatal("fakeNextHop: timeout waiting for a proxied request")
		return nil
	}
}

// ── simpleCaller: sends non-dialog SIP requests (OPTIONS, REGISTER, …) ────────

type simpleCaller struct {
	cli *sipgo.Client
}

func newSimpleCaller(t *testing.T) *simpleCaller {
	t.Helper()
	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("simpleCaller UA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("simpleCaller server: %v", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("simpleCaller client: %v", err)
	}
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("simpleCaller listen: %v", err)
	}
	go srv.ServeUDP(l) //nolint:errcheck
	t.Cleanup(func() { l.Close() })
	return &simpleCaller{cli: cli}
}

// send issues a non-dialog request and returns the first final response.
func (c *simpleCaller) send(ctx context.Context, method sip.RequestMethod, targetURI string) (*sip.Response, error) {
	var uri sip.Uri
	if err := sip.ParseUri(targetURI, &uri); err != nil {
		return nil, err
	}
	tx, err := c.cli.TransactionRequest(ctx, sip.NewRequest(method, uri))
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
