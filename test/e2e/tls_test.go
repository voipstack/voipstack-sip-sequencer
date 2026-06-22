//go:build e2e

package e2e

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// ── real certificates (AGENTS.md: real fakes, no mocks) ──────────────────────

type certKey struct {
	cert *x509.Certificate
	der  []byte
	key  *rsa.PrivateKey
}

// mintCert creates a certificate signed by parent (self-signed when parent is nil).
// Date.now() is unavailable in workflow scripts but fine in ordinary test code.
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

// clientProfile writes the sequencer's client cert/key and a CA bundle to a temp
// dir and returns a yamlTLSProfile pointing at them. The engine validates the
// remote against the CA and presents the client cert for mTLS.
func clientProfile(t *testing.T, cliCK, caCK *certKey) yamlTLSProfile {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")
	caPath := filepath.Join(dir, "ca.crt")
	writePEM(t, certPath, cliCK.certPEM())
	writePEM(t, keyPath, cliCK.keyPEM())
	writePEM(t, caPath, caCK.certPEM())
	return yamlTLSProfile{Cert: certPath, Key: keyPath, CA: caPath, MinVersion: "tlsv1.2"}
}

// serverTLSConf builds the fake app's server-side TLS config, optionally requiring
// a client cert signed by caCK (mTLS).
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

// newFakeUASTLS is a fakeUAS reachable only over TLS: a request arrives only if the
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
	f.installHandlers(srv, dsc)

	go srv.ServeTLS(l) //nolint:errcheck
	t.Cleanup(func() { l.Close() })

	return f
}

// ── scenarios ────────────────────────────────────────────────────────────────

// Given an app configured transport: tls with a trusted server cert; When a call
// arrives; Then the app leg completes over TLS and the call connects end to end.
func TestAppLegOverTLSConnects(t *testing.T) {
	caCK := mintCert(t, "e2e-ca", nil, true, nil, nil)
	srvCK := mintCert(t, "app-server", caCK, false, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")})
	cliCK := mintCert(t, "sequencer-client", caCK, false, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)

	app := newFakeUASTLS(t, serverTLSConf(srvCK, false, nil))
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	serveApp(t, app, "secure", nil, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{{
		Name: "secure", URI: app.sipURI(), Transport: "tls", TLSProfile: "appsec",
		Media: "none", OnFailure: "abort",
	}})
	cfg.TLSProfiles = map[string]yamlTLSProfile{"appsec": clientProfile(t, cliCK, caCK)}
	s := startReady(t, cfg)

	establish(t, caller, s.sipListen)
}

// Given an app that requires a client certificate (mTLS); When a call arrives;
// Then the sequencer presents its profile's client cert and the leg is accepted.
func TestAppLegMutualTLS(t *testing.T) {
	caCK := mintCert(t, "e2e-ca", nil, true, nil, nil)
	srvCK := mintCert(t, "app-server", caCK, false, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")})
	cliCK := mintCert(t, "sequencer-client", caCK, false, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)

	app := newFakeUASTLS(t, serverTLSConf(srvCK, true, caCK)) // requires client cert
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	serveApp(t, app, "secure", nil, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{{
		Name: "secure", URI: app.sipURI(), Transport: "tls", TLSProfile: "appsec",
		Media: "none", OnFailure: "abort",
	}})
	cfg.TLSProfiles = map[string]yamlTLSProfile{"appsec": clientProfile(t, cliCK, caCK)}
	s := startReady(t, cfg)

	establish(t, caller, s.sipListen)
}

// Given a TLS app with verify_peer enabled whose server cert is signed by an untrusted
// CA, with on_failure: skip; When a call arrives; Then the handshake fails, the app
// never sees the INVITE, and the call proceeds to the PBX. (Validation is opt-in: the
// default relaxed/encrypt-only posture would accept the untrusted cert.)
func TestUntrustedTLSAppSkipped(t *testing.T) {
	trustedCA := mintCert(t, "trusted-ca", nil, true, nil, nil)
	cliCK := mintCert(t, "sequencer-client", trustedCA, false, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	rogueCA := mintCert(t, "rogue-ca", nil, true, nil, nil)
	rogueSrv := mintCert(t, "rogue-server", rogueCA, false, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")})

	app := newFakeUASTLS(t, serverTLSConf(rogueSrv, false, nil))
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{{
		Name: "secure", URI: app.sipURI(), Transport: "tls", TLSProfile: "appsec",
		Media: "none", OnFailure: "skip",
	}})
	prof := clientProfile(t, cliCK, trustedCA)
	prof.VerifyPeer = true // opt into validation so the untrusted server cert is rejected
	cfg.TLSProfiles = map[string]yamlTLSProfile{"appsec": prof}
	s := startReady(t, cfg)

	// Skipped on the untrusted handshake — the PBX answers and the call connects.
	establish(t, caller, s.sipListen)

	app.noInvite(t, 300*time.Millisecond)
}
