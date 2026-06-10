package b2bua

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
	"github.com/voipstack/voipstack-sip-sequencer/internal/tlsprov"
)

// ── outbound TLS fixtures (real certs, real fakes) ───────────────────────────

// outboundFixture mints a CA that signs both a 127.0.0.1 server cert and the
// sequencer's client cert, writes the client material + CA bundle to a temp dir, and
// returns a resolved outbound profile (the engine validates the server against the CA
// and presents the client cert for mTLS) plus a server-side *tls.Config bound to the
// server cert. requireClientCert turns the server into an mTLS verifier.
func outboundFixture(t *testing.T, requireClientCert bool) (config.ResolvedTLSProfile, *tls.Config) {
	t.Helper()
	caCK := mintCert(t, "outbound-ca", nil, true, nil, nil)
	srvCK := mintCert(t, "app-server", caCK, false, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")})
	cliCK := mintCert(t, "sequencer-client", caCK, false, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)

	rp := writeClientProfile(t, "outbound", cliCK, caCK)
	srvConf := serverTLSConf(srvCK, requireClientCert, caCK)
	return rp, srvConf
}

// writeClientProfile writes the client cert+key and CA bundle to a temp dir and returns
// the resolved profile pointing at them.
func writeClientProfile(t *testing.T, name string, cliCK, caCK *certKey) config.ResolvedTLSProfile {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")
	caPath := filepath.Join(dir, "ca.crt")
	writePEM(t, certPath, cliCK.certPEM())
	writePEM(t, keyPath, cliCK.keyPEM())
	writePEM(t, caPath, caCK.certPEM())
	return config.ResolvedTLSProfile{
		Name:        name,
		Cert:        certPath,
		Key:         keyPath,
		CA:          caPath,
		MinVersion:  config.TLSv12,
		VerifyDates: true,
	}
}

// serverTLSConf builds the fake remote's server-side TLS config from srvCK, optionally
// requiring a client cert signed by caCK (mTLS).
func serverTLSConf(srvCK *certKey, requireClientCert bool, caCK *certKey) *tls.Config {
	conf := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{srvCK.der}, PrivateKey: srvCK.key}},
		MinVersion:   tls.VersionTLS12,
	}
	if requireClientCert {
		pool := x509.NewCertPool()
		pool.AddCert(caCK.cert)
		conf.ClientCAs = pool
		conf.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return conf
}

// newFakeUASTLS is newFakeUASTCP over a TLS listener: a request reaches it only if the
// sender completes a TLS handshake against srvConf. It is the external TLS peer.
func newFakeUASTLS(t *testing.T, srvConf *tls.Config) *fakeUAS {
	t.Helper()

	ua, err := sipgo.NewUA()
	if err != nil {
		t.Fatalf("fakeUASTLS UA: %v", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		t.Fatalf("fakeUASTLS server: %v", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("fakeUASTLS client: %v", err)
	}

	l, err := tls.Listen("tcp", "127.0.0.1:0", srvConf)
	if err != nil {
		t.Fatalf("fakeUASTLS listen: %v", err)
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

	go srv.ServeTLS(l) //nolint:errcheck
	t.Cleanup(func() { l.Close() })

	return f
}

// outboundTLSConfig wires the single app to dial over TLS with rp, leaving the next hop
// plain (the pbx fake on UDP).
func outboundTLSConfig(listenAddr, appURI, pbxURI string, rp config.ResolvedTLSProfile) config.Config {
	cfg := testConfig(listenAddr, appURI, pbxURI)
	cfg.Sequence[0].Transport = config.TransportTLS
	cfg.Sequence[0].TLSProfile = rp.Name
	cfg.Sequence[0].Resolved = &rp
	return cfg
}

// blackHoleAddr returns the address of a TCP listener that accepts connections but never
// completes the TLS handshake, so a dial against it hangs until its context expires.
func blackHoleAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("black-hole listen: %v", err)
	}
	held := make(chan net.Conn, 16)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			held <- c // hold the conn open; never speak TLS
		}
	}()
	t.Cleanup(func() {
		l.Close()
		close(held)
		for c := range held {
			c.Close()
		}
	})
	return l.Addr().String()
}

// ── behavior tests ───────────────────────────────────────────────────────────

// AC1: an app with transport: tls is dialed over TLS — its leg reaches a TLS-only fake
// (a plain dial would never complete the handshake), and the call connects end-to-end.
// The single transport field selects the path.
func TestAppLegSwitchesTLSvsPlain(t *testing.T) {
	rp, srvConf := outboundFixture(t, false)
	app := newFakeUASTLS(t, srvConf)
	pbx := newFakeUAS(t)

	listenAddr := freeAddr(t)
	startEngineTLS(t, outboundTLSConfig(listenAddr, app.sipURI(), pbx.sipURI(), rp))

	inviteAndWait200(t, newFakeUAC(t), "sip:"+listenAddr, app, pbx)
}

// AC2: when the remote requires a client certificate (mTLS), the sequencer presents the
// profile's client cert and the leg is accepted.
func TestOutboundMutualTLSPresentsClientCert(t *testing.T) {
	rp, srvConf := outboundFixture(t, true)
	app := newFakeUASTLS(t, srvConf)
	pbx := newFakeUAS(t)

	listenAddr := freeAddr(t)
	startEngineTLS(t, outboundTLSConfig(listenAddr, app.sipURI(), pbx.sipURI(), rp))

	inviteAndWait200(t, newFakeUAC(t), "sip:"+listenAddr, app, pbx)
}

// AC3: a remote whose server cert is signed by a CA the profile does not trust is
// refused; with on_failure: skip the failure is confined to that app (it never reaches
// the fake) and the call proceeds to the next hop.
func TestUntrustedRemoteRefusedThenSkip(t *testing.T) {
	// Client profile trusts caCK; the server presents a cert from a different CA.
	caCK := mintCert(t, "trusted-ca", nil, true, nil, nil)
	cliCK := mintCert(t, "sequencer-client", caCK, false, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	rogueCA := mintCert(t, "rogue-ca", nil, true, nil, nil)
	rogueSrv := mintCert(t, "rogue-server", rogueCA, false, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")})

	rp := writeClientProfile(t, "outbound", cliCK, caCK)
	app := newFakeUASTLS(t, serverTLSConf(rogueSrv, false, nil))
	pbx := newFakeUAS(t)

	listenAddr := freeAddr(t)
	cfg := outboundTLSConfig(listenAddr, app.sipURI(), pbx.sipURI(), rp)
	cfg.Sequence[0].OnFailure = config.FailureSkip
	eng := startEngineTLS(t, cfg)

	// The app is skipped (untrusted) so only the pbx leg answers — driving the call
	// manually rather than via inviteAndWait200, which would block on an app INVITE
	// that never arrives.
	ctx := context.Background()
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()
	sess, err := newFakeUAC(t).invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200 after skip, got %d", sess.InviteResponse.StatusCode)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	app.noInvite(t, 200*time.Millisecond)
	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call after skip, got %d", n)
	}
}

// AC4: a connect_timeout abandons a non-responsive TLS dial fast instead of hanging. The
// app dials a black hole with on_failure: abort; the inbound caller gets a final
// response well within the leg timeout.
func TestConnectTimeoutFailsFast(t *testing.T) {
	rp, _ := outboundFixture(t, false)
	rp.ConnectTimeout = 200 * time.Millisecond

	pbx := newFakeUAS(t)
	listenAddr := freeAddr(t)
	cfg := outboundTLSConfig(listenAddr, "sip:"+blackHoleAddr(t), pbx.sipURI(), rp)
	cfg.Sequence[0].OnFailure = config.FailureAbort
	startEngineTLS(t, cfg)

	caller := newFakeUAC(t)
	ctx := context.Background()
	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	start := time.Now()
	done := make(chan struct{})
	go func() {
		_ = sess.WaitAnswer(ctx, sipgo.AnswerOptions{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dial did not fail fast: caller still waiting after 5s")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("connect timeout did not bound the dial: %s", elapsed)
	}
	if sess.InviteResponse == nil || sess.InviteResponse.StatusCode == 200 {
		t.Fatalf("expected a failure response, got %v", sess.InviteResponse)
	}
}

// AC5: an app and the next hop naming the same profile share one dialer — one client
// context built, one certificate loaded.
func TestProfileReusedReusesCert(t *testing.T) {
	rp, _ := outboundFixture(t, false)

	cfg := testConfig(freeAddr(t), "sip:127.0.0.1:5080", "sip:127.0.0.1:5090")
	cfg.Sequence[0].Transport = config.TransportTLS
	cfg.Sequence[0].TLSProfile = rp.Name
	cfg.Sequence[0].Resolved = &rp
	cfg.NextHop.Transport = config.TransportTLS
	cfg.NextHop.TLSProfile = rp.Name
	cfg.NextHop.Resolved = &rp

	cp := &countingProvider{Provider: tlsprov.NewStdProvider(nil)}
	eng, err := New(cfg, WithTLSProvider(cp))
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if n := len(eng.tlsDialers); n != 1 {
		t.Fatalf("expected 1 shared dialer, got %d", n)
	}
	if cp.clientCalls != 1 {
		t.Fatalf("expected ClientConfig built once, got %d", cp.clientCalls)
	}
}

// AC6: a plain next hop (no TLS) dials exactly as today and the engine builds no TLS
// dialers.
func TestPlainNextHopDialsPlain(t *testing.T) {
	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)

	listenAddr := freeAddr(t)
	eng := startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)

	if n := len(eng.tlsDialers); n != 0 {
		t.Fatalf("expected no TLS dialers for a plain config, got %d", n)
	}
	completeCall(t, newFakeUAC(t), "sip:"+listenAddr, app, pbx)
	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call, got %d", n)
	}
}

// Shutdown closes the per-profile outbound TLS UAs and reports no error.
func TestShutdownClosesTLSUAs(t *testing.T) {
	rp, _ := outboundFixture(t, false)

	cfg := outboundTLSConfig(freeAddr(t), "sip:127.0.0.1:5080", "sip:127.0.0.1:5090", rp)
	eng, err := New(cfg, WithTLSProvider(tlsprov.NewStdProvider(nil)))
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if len(eng.tlsUAs) != 1 {
		t.Fatalf("expected 1 owned TLS UA, got %d", len(eng.tlsUAs))
	}
	if err := eng.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// Idempotent: a second shutdown must not panic or error.
	if err := eng.Shutdown(); err != nil {
		t.Fatalf("second shutdown: %v", err)
	}
}

// countingProvider wraps a Provider to count ClientConfig calls, proving one client
// context (and so one loaded cert) per distinct profile.
type countingProvider struct {
	tlsprov.Provider
	clientCalls int
}

func (c *countingProvider) ClientConfig(rp config.ResolvedTLSProfile) (*tls.Config, error) {
	c.clientCalls++
	return c.Provider.ClientConfig(rp)
}
