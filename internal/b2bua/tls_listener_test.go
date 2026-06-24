package b2bua

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
	"github.com/voipstack/voipstack-sip-sequencer/internal/tlsprov"
)

// ── real-cert helpers (AGENTS.md: real fakes, no mocks) ──────────────────────

type certKey struct {
	cert *x509.Certificate
	der  []byte
	key  *rsa.PrivateKey
}

// mintCert creates a certificate signed by parent (self-signed when parent is nil).
func mintCert(t *testing.T, cn string, parent *certKey, isCA bool, ekus []x509.ExtKeyUsage, ips []net.IP) *certKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           ekus,
		IPAddresses:           ips,
		IsCA:                  isCA,
		BasicConstraintsValid: true,
	}
	if isCA {
		tmpl.KeyUsage |= x509.KeyUsageCertSign
	}
	signerCert, signerKey := tmpl, key
	if parent != nil {
		signerCert, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return &certKey{cert: parsed, der: der, key: key}
}

func (ck *certKey) certPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ck.der})
}

func (ck *certKey) keyPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(ck.key)})
}

func writePEM(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// serverProfile mints a CA + a 127.0.0.1 server cert, writes them to a temp dir, and
// returns a resolved listener profile. When verifyPeer is set the profile enables
// mutual TLS, trusting clients signed by the same CA.
func serverProfile(t *testing.T, verifyPeer bool) config.ResolvedTLSProfile {
	t.Helper()
	dir := t.TempDir()
	caCK := mintCert(t, "test-ca", nil, true, nil, nil)
	srvCK := mintCert(t, "sequencer", caCK, false, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")})

	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	caPath := filepath.Join(dir, "ca.crt")
	writePEM(t, certPath, srvCK.certPEM())
	writePEM(t, keyPath, srvCK.keyPEM())
	writePEM(t, caPath, caCK.certPEM())

	rp := config.ResolvedTLSProfile{
		Name:        "listener",
		Cert:        certPath,
		Key:         keyPath,
		MinVersion:  config.TLSv12,
		VerifyDates: true,
	}
	if verifyPeer {
		rp.CA = caPath
		rp.VerifyPeer = true
	}
	return rp
}

// tlsConfig builds a config with both a plain UDP listener and a TLS listener bound
// to rp, plus the app/pbx sequence wiring of testConfig.
func tlsConfig(plainAddr, tlsAddr, appURI, pbxURI string, rp config.ResolvedTLSProfile) config.Config {
	cfg := testConfig(plainAddr, appURI, pbxURI)
	cfg.TLS = config.TLS{Listen: tlsAddr, TLSProfile: rp.Name, Resolved: &rp}
	return cfg
}

// ── TLS-capable fake caller (the external peer — the only fake) ───────────────

// newFakeUACTLS is newFakeUAC dialing over TLS: its UA carries the client-side TLS
// config used for every TLS dial. It needs no inbound listener because in-dialog
// responses return on the established TLS connection.
func newFakeUACTLS(t *testing.T, clientConf *tls.Config) *fakeUAC {
	t.Helper()
	ua, err := sipgo.NewUA(sipgo.WithUserAgenTLSConfig(clientConf))
	if err != nil {
		t.Fatalf("fakeUACTLS UA: %v", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("fakeUACTLS client: %v", err)
	}
	contact := sip.ContactHeader{Address: sip.Uri{Host: "127.0.0.1", Port: 5060}}
	return &fakeUAC{dcc: sipgo.NewDialogClientCache(cli, contact)}
}

// ── engine starter with a TLS provider ───────────────────────────────────────

func startEngineTLS(t *testing.T, cfg config.Config) *Engine {
	t.Helper()
	provider := tlsprov.NewStdProvider(nil)
	eng, err := New(cfg, WithTLSProvider(provider))
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	waitEngineReady(t, eng, cfg)
	if cfg.TLS.Listen != "" {
		waitPortOpen(t, cfg.TLS.Listen)
	}
	return eng
}

// waitPortOpen blocks until a raw TCP connection to addr succeeds, proving the TLS
// listener has bound (the plain ready signal only covers the UDP socket).
func waitPortOpen(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("tls port %q never opened", addr)
}

// completeCall drives one INVITE through the engine to a 200/ACK, with the app and
// pbx fakes answering their legs. It returns once the caller has ACKed.
func completeCall(t *testing.T, caller *fakeUAC, target string, app, pbx *fakeUAS) {
	t.Helper()
	sess := inviteAndWait200(t, caller, target, app, pbx)
	if err := sess.Ack(context.Background()); err != nil {
		t.Fatalf("ACK: %v", err)
	}
}

// inviteAndWait200 sends one INVITE through the engine, answers both legs, and
// returns the caller session once a 200 OK arrives. It stops at the answer (no ACK):
// in-dialog request routing back to the engine follows the Contact header, whose
// per-transport rewriting is out of this story's scope.
func inviteAndWait200(t *testing.T, caller *fakeUAC, target string, app, pbx *fakeUAS) *sipgo.DialogClientSession {
	t.Helper()
	ctx := context.Background()
	go func() {
		dss := app.waitInvite(t, 3*time.Second)
		_ = dss.Respond(180, "Ringing", nil)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()

	sess, err := caller.invite(ctx, target, []byte(testSDP))
	if err != nil {
		t.Fatalf("invite %s: %v", target, err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer %s: %v", target, err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", sess.InviteResponse.StatusCode)
	}
	return sess
}

// dialTLSNoClientCert opens a raw TLS connection presenting no client certificate
// and returns the first error observed. It reads one byte after the handshake
// because under TLS 1.3 a server's client-cert rejection surfaces only on the first
// read, not from Handshake itself. It is the external "untrusted client" peer.
func dialTLSNoClientCert(addr string) error {
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", addr,
		&tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	return err
}

// ── behavior tests ───────────────────────────────────────────────────────────

// AC1/AC2: with both listeners active, a plain UDP caller and a TLS caller each
// complete an INVITE end-to-end through the same handlers — only the transport
// differs.
func TestTLSAndPlainListenersActive(t *testing.T) {
	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)

	plainAddr := freeAddr(t)
	tlsAddr := freeAddr(t)
	eng := startEngineTLS(t, tlsConfig(plainAddr, tlsAddr, app.sipURI(), pbx.sipURI(), serverProfile(t, false)))

	// Plain UDP caller completes a full call (registered after ACK).
	completeCall(t, newFakeUAC(t), "sip:"+plainAddr, app, pbx)
	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call after plain INVITE, got %d", n)
	}

	// TLS caller drives the same handlers and gets a 200 OK answer end-to-end.
	tlsCaller := newFakeUACTLS(t, &tls.Config{InsecureSkipVerify: true})
	inviteAndWait200(t, tlsCaller, "sip:"+tlsAddr+";transport=tls", app, pbx)
}

// AC3: an mTLS listener rejects a client presenting no certificate at the handshake,
// while a concurrent plain UDP call on the same engine still completes — the
// rejection is confined to the TLS connection.
func TestMTLSRejectsUntrustedClient(t *testing.T) {
	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)

	plainAddr := freeAddr(t)
	tlsAddr := freeAddr(t)
	startEngineTLS(t, tlsConfig(plainAddr, tlsAddr, app.sipURI(), pbx.sipURI(), serverProfile(t, true)))

	// No client certificate → the mTLS handshake must fail, and the engine never
	// sees an INVITE on the rejected connection.
	if err := dialTLSNoClientCert(tlsAddr); err == nil {
		t.Fatal("expected mTLS handshake rejection, got nil error")
	}
	app.noInvite(t, 200*time.Millisecond)

	// The plain listener is unaffected by the rejected TLS handshake.
	completeCall(t, newFakeUAC(t), "sip:"+plainAddr, app, pbx)
}

// AC4: a rejected handshake is logged with the peer address and a reason, and the
// log carries no certificate or private-key material.
func TestHandshakeFailureLoggedNoSecrets(t *testing.T) {
	cap := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	t.Cleanup(func() { slog.SetDefault(prev) })

	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)
	tlsAddr := freeAddr(t)
	startEngineTLS(t, tlsConfig(freeAddr(t), tlsAddr, app.sipURI(), pbx.sipURI(), serverProfile(t, true)))

	_ = dialTLSNoClientCert(tlsAddr)

	rec := cap.await(t, "tls handshake rejected", 3*time.Second)
	if peer, _ := rec.attr("peer"); peer == "" {
		t.Fatal("handshake log missing peer address")
	}
	if reason, _ := rec.attr("reason"); reason == "" {
		t.Fatal("handshake log missing reason")
	}
	if blob := strings.ToUpper(rec.String()); strings.Contains(blob, "BEGIN CERTIFICATE") || strings.Contains(blob, "PRIVATE KEY") {
		t.Fatalf("handshake log leaked certificate material: %s", rec.String())
	}
}

// AC6: a config with no tls block opens no TLS listener — the engine holds no server
// TLS context and only the plain path runs.
func TestNoTLSListenNoTLSPort(t *testing.T) {
	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)
	cfg := testConfig(freeAddr(t), app.sipURI(), pbx.sipURI())

	eng, err := New(cfg, WithTLSProvider(tlsprov.NewStdProvider(nil)))
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if eng.tlsServerConf != nil {
		t.Fatal("expected nil tlsServerConf with no tls.listen")
	}
}

// Teardown: after Shutdown the TLS port no longer accepts connections.
func TestShutdownClosesTLSListener(t *testing.T) {
	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)
	tlsAddr := freeAddr(t)
	eng := startEngineTLS(t, tlsConfig(freeAddr(t), tlsAddr, app.sipURI(), pbx.sipURI(), serverProfile(t, false)))

	if err := eng.Shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", tlsAddr, 200*time.Millisecond)
		if err != nil {
			return // refused → listener closed
		}
		c.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("tls port %q still accepting after shutdown", tlsAddr)
}

// R1 fail-fast: a tls.listen with no provider, or with no resolved profile, aborts
// New rather than starting degraded.
func TestNewFailsFastOnTLSMisconfig(t *testing.T) {
	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)

	cfg := tlsConfig(freeAddr(t), freeAddr(t), app.sipURI(), pbx.sipURI(), serverProfile(t, false))
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error: tls.listen configured but no provider")
	}

	noResolved := cfg
	noResolved.TLS = config.TLS{Listen: cfg.TLS.Listen, TLSProfile: "listener", Resolved: nil}
	if _, err := New(noResolved, WithTLSProvider(tlsprov.NewStdProvider(nil))); err == nil {
		t.Fatal("expected error: tls.listen has no resolved profile")
	}
}

// ── slog capture handler ──────────────────────────────────────────────────────

type capRecord struct {
	msg   string
	attrs map[string]string
}

func (r capRecord) attr(k string) (string, bool) { v, ok := r.attrs[k]; return v, ok }

func (r capRecord) String() string {
	var b strings.Builder
	b.WriteString(r.msg)
	for k, v := range r.attrs {
		b.WriteString(" " + k + "=" + v)
	}
	return b.String()
}

type captureHandler struct {
	mu      sync.Mutex
	records []capRecord
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capRecord{msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.records = append(h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) await(t *testing.T, msg string, timeout time.Duration) capRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for _, r := range h.records {
			if r.msg == msg {
				h.mu.Unlock()
				return r
			}
		}
		h.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("log record %q not seen within %s", msg, timeout)
	return capRecord{}
}
