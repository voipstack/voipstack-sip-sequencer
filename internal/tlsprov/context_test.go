package tlsprov

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
	"strings"
	"testing"
	"time"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// --- cert minting helpers (real certs, no mocks) -----------------------------

var (
	epoch      = time.Unix(0, 0)
	farFuture  = time.Unix(1<<31, 0)
	pastExpiry = time.Unix(1000, 0) // expired relative to "now"
)

type certKey struct {
	cert *x509.Certificate
	key  *rsa.PrivateKey
	der  []byte
}

// mint creates a certificate signed by parent (self-signed when parent is nil).
func mint(t *testing.T, cn string, parent *certKey, isCA bool, notAfter time.Time, ekus []x509.ExtKeyUsage, dns []string) *certKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    epoch,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  ekus,
		DNSNames:     dns,
	}
	if isCA {
		tmpl.IsCA = true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
		tmpl.BasicConstraintsValid = true
	}
	signerCert, signerKey := tmpl, key
	if parent != nil {
		signerCert, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signerCert, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatalf("create cert %q: %v", cn, err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert %q: %v", cn, err)
	}
	return &certKey{cert: parsed, key: key, der: der}
}

func (ck *certKey) certPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ck.der})
}

func (ck *certKey) keyPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(ck.key)})
}

func serverAuth(t *testing.T, cn string, parent *certKey, dns string) *certKey {
	return mint(t, cn, parent, false, farFuture, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{dns})
}

func clientAuth(t *testing.T, cn string, parent *certKey) *certKey {
	return mint(t, cn, parent, false, farFuture, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
}

func ca(t *testing.T, cn string) *certKey {
	return mint(t, cn, nil, true, farFuture, nil, nil)
}

// pool builds an x509 trust pool from CA certs.
func pool(cas ...*certKey) *x509.CertPool {
	p := x509.NewCertPool()
	for _, c := range cas {
		p.AddCert(c.cert)
	}
	return p
}

// peerCert builds a tls.Certificate presenting leaf followed by any chain certs.
func peerCert(leaf *certKey, chain ...*certKey) tls.Certificate {
	c := tls.Certificate{PrivateKey: leaf.key, Leaf: leaf.cert, Certificate: [][]byte{leaf.der}}
	for _, x := range chain {
		c.Certificate = append(c.Certificate, x.der)
	}
	return c
}

// writeProfile writes leaf cert+key (and optional CA) to a temp dir and returns a
// resolved profile with secure defaults applied, optionally mutated.
func writeProfile(t *testing.T, name string, leaf *certKey, trust *certKey, mutate func(*config.ResolvedTLSProfile)) config.ResolvedTLSProfile {
	t.Helper()
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, leaf.certPEM(), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyPath, leaf.keyPEM(), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	rp := config.ResolvedTLSProfile{
		Name:        name,
		Cert:        certPath,
		Key:         keyPath,
		MinVersion:  config.TLSv12,
		VerifyDepth: 2,
		VerifyDates: true,
	}
	if trust != nil {
		caPath := filepath.Join(dir, "ca.pem")
		if err := os.WriteFile(caPath, trust.certPEM(), 0o600); err != nil {
			t.Fatalf("write ca: %v", err)
		}
		rp.CA = caPath
	}
	if mutate != nil {
		mutate(&rp)
	}
	return rp
}

func withSNI(cfg *tls.Config, name string) *tls.Config {
	c := cfg.Clone()
	c.ServerName = name
	return c
}

// handshake drives an in-process TLS handshake over net.Pipe and returns each
// side's error.
func handshake(t *testing.T, serverCfg, clientCfg *tls.Config) (clientState tls.ConnectionState, srvErr, cliErr error) {
	t.Helper()
	cConn, sConn := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	_ = cConn.SetDeadline(deadline)
	_ = sConn.SetDeadline(deadline)

	srv := tls.Server(sConn, serverCfg)
	cli := tls.Client(cConn, clientCfg)

	done := make(chan error, 1)
	go func() {
		done <- srv.Handshake()
		_ = srv.Close()
	}()
	cliErr = cli.Handshake()
	clientState = cli.ConnectionState()
	_ = cli.Close()
	srvErr = <-done
	return clientState, srvErr, cliErr
}

// --- AC1: secure default, one-way handshake ----------------------------------

func TestDefaultServerOneWayHandshake(t *testing.T) {
	root := ca(t, "root")
	server := serverAuth(t, "server", root, "server.test")

	p := NewStdProvider(nil)
	srvCfg, err := p.ServerConfig(writeProfile(t, "default", server, nil, nil))
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	if srvCfg.ClientAuth != tls.NoClientCert {
		t.Fatalf("expected NoClientCert, got %v", srvCfg.ClientAuth)
	}

	cliCfg := &tls.Config{RootCAs: pool(root), ServerName: "server.test"}
	_, srvErr, cliErr := handshake(t, srvCfg, cliCfg)
	if srvErr != nil || cliErr != nil {
		t.Fatalf("expected one-way handshake to succeed: srv=%v cli=%v", srvErr, cliErr)
	}
}

// --- AC2: verify_peer enforces mTLS ------------------------------------------

func TestVerifyPeerEnforcesMTLS(t *testing.T) {
	root := ca(t, "root")
	server := serverAuth(t, "server", root, "server.test")
	client := clientAuth(t, "phone", root)

	p := NewStdProvider(nil)
	srvCfg, err := p.ServerConfig(writeProfile(t, "mtls", server, root, func(rp *config.ResolvedTLSProfile) {
		rp.VerifyPeer = true
	}))
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	if srvCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("expected RequireAndVerifyClientCert, got %v", srvCfg.ClientAuth)
	}

	// CA-signed client accepted.
	withCert := &tls.Config{Certificates: []tls.Certificate{peerCert(client)}, RootCAs: pool(root), ServerName: "server.test"}
	if _, srvErr, cliErr := handshake(t, srvCfg, withCert); srvErr != nil || cliErr != nil {
		t.Fatalf("expected mTLS client accepted: srv=%v cli=%v", srvErr, cliErr)
	}

	// No-cert client rejected.
	noCert := &tls.Config{RootCAs: pool(root), ServerName: "server.test"}
	if _, srvErr, _ := handshake(t, srvCfg, noCert); srvErr == nil {
		t.Fatal("expected server to reject client with no certificate")
	}
}

// --- AC3: verify_subjects restricts peers ------------------------------------

func TestVerifySubjectsRestrictsPeers(t *testing.T) {
	root := ca(t, "root")
	clientLeaf := clientAuth(t, "agent", root)

	allowed := serverAuth(t, "phone.internal", root, "phone.internal")
	rejected := serverAuth(t, "other", root, "other.test")

	p := NewStdProvider(nil)
	rp := writeProfile(t, "subjects", clientLeaf, root, func(rp *config.ResolvedTLSProfile) {
		rp.VerifySubjects = []string{"CN=phone.internal"}
	})
	cliCfg, err := p.ClientConfig(rp)
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}

	accSrv := &tls.Config{Certificates: []tls.Certificate{peerCert(allowed)}}
	if _, _, cliErr := handshake(t, accSrv, withSNI(cliCfg, "phone.internal")); cliErr != nil {
		t.Fatalf("expected allowed subject accepted: %v", cliErr)
	}

	rejSrv := &tls.Config{Certificates: []tls.Certificate{peerCert(rejected)}}
	if _, _, cliErr := handshake(t, rejSrv, withSNI(cliCfg, "other.test")); cliErr == nil {
		t.Fatal("expected disallowed subject rejected")
	}
}

// --- AC4: verify_depth caps chain --------------------------------------------

func TestVerifyDepthCapsChain(t *testing.T) {
	root := ca(t, "root")
	inter := mint(t, "inter", root, true, farFuture, nil, nil)
	clientLeaf := clientAuth(t, "agent", root)
	server := serverAuth(t, "server", inter, "server.test") // one intermediate

	p := NewStdProvider(nil)
	srvCfg := &tls.Config{Certificates: []tls.Certificate{peerCert(server, inter)}}

	// verify_depth:0 rejects a chain with one intermediate.
	rp0 := writeProfile(t, "depth0", clientLeaf, root, func(rp *config.ResolvedTLSProfile) {
		rp.VerifyDepth = 0
	})
	cfg0, err := p.ClientConfig(rp0)
	if err != nil {
		t.Fatalf("ClientConfig depth0: %v", err)
	}
	if _, _, cliErr := handshake(t, srvCfg, withSNI(cfg0, "server.test")); cliErr == nil {
		t.Fatal("expected verify_depth:0 to reject one-intermediate chain")
	}

	// verify_depth:1 accepts it.
	rp1 := writeProfile(t, "depth1", clientLeaf, root, func(rp *config.ResolvedTLSProfile) {
		rp.VerifyDepth = 1
	})
	cfg1, err := p.ClientConfig(rp1)
	if err != nil {
		t.Fatalf("ClientConfig depth1: %v", err)
	}
	if _, _, cliErr := handshake(t, srvCfg, withSNI(cfg1, "server.test")); cliErr != nil {
		t.Fatalf("expected verify_depth:1 to accept one-intermediate chain: %v", cliErr)
	}
}

// --- AC5: min_version enforced -----------------------------------------------

func TestMinVersionEnforced(t *testing.T) {
	root := ca(t, "root")
	server := serverAuth(t, "server", root, "server.test")

	p := NewStdProvider(nil)
	srvCfg, err := p.ServerConfig(writeProfile(t, "minver", server, nil, nil))
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}

	tls11Client := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // G402: deliberately isolating the version-floor behavior
		MinVersion:         tls.VersionTLS10,
		MaxVersion:         tls.VersionTLS11,
	}
	if _, srvErr, _ := handshake(t, srvCfg, tls11Client); srvErr == nil {
		t.Fatal("expected TLS 1.1 client to be rejected by a TLS 1.2 floor")
	}
}

// --- AC6: ciphers restricted to TLS 1.2 --------------------------------------

func TestCiphersTLS12Only(t *testing.T) {
	const suiteName = "TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256"
	suiteID := tls.CipherSuites()[0].ID
	for _, s := range tls.CipherSuites() {
		if s.Name == suiteName {
			suiteID = s.ID
		}
	}

	root := ca(t, "root")
	server := serverAuth(t, "server", root, "server.test")

	p := NewStdProvider(nil)
	srvCfg, err := p.ServerConfig(writeProfile(t, "ciphers", server, nil, func(rp *config.ResolvedTLSProfile) {
		rp.Ciphers = []string{suiteName}
	}))
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}

	// TLS 1.2 handshake negotiates exactly the allowed suite.
	cli12 := &tls.Config{
		RootCAs:      pool(root),
		ServerName:   "server.test",
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS12,
		CipherSuites: []uint16{suiteID},
	}
	state, srvErr, cliErr := handshake(t, srvCfg, cli12)
	if srvErr != nil || cliErr != nil {
		t.Fatalf("expected TLS 1.2 handshake: srv=%v cli=%v", srvErr, cliErr)
	}
	if state.CipherSuite != suiteID {
		t.Fatalf("expected suite %x, got %x", suiteID, state.CipherSuite)
	}

	// TLS 1.3 ignores the TLS 1.2 allowlist (Go fixes 1.3 suites).
	cli13 := &tls.Config{
		RootCAs:    pool(root),
		ServerName: "server.test",
		MinVersion: tls.VersionTLS13,
		MaxVersion: tls.VersionTLS13,
	}
	if _, srvErr, cliErr := handshake(t, srvCfg, cli13); srvErr != nil || cliErr != nil {
		t.Fatalf("expected TLS 1.3 handshake to ignore allowlist: srv=%v cli=%v", srvErr, cliErr)
	}
}

// --- AC7: verify_dates toggle ------------------------------------------------

func TestVerifyDatesToggle(t *testing.T) {
	root := ca(t, "root")
	other := ca(t, "untrusted")
	clientLeaf := clientAuth(t, "agent", root)
	expired := mint(t, "expired", root, false, pastExpiry, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{"server.test"})
	expiredUntrusted := mint(t, "expired", other, false, pastExpiry, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{"server.test"})

	p := NewStdProvider(nil)

	// verify_dates:true rejects an expired remote.
	strict, err := p.ClientConfig(writeProfile(t, "dates-strict", clientLeaf, root, nil))
	if err != nil {
		t.Fatalf("ClientConfig strict: %v", err)
	}
	expSrv := &tls.Config{Certificates: []tls.Certificate{peerCert(expired)}}
	if _, _, cliErr := handshake(t, expSrv, withSNI(strict, "server.test")); cliErr == nil {
		t.Fatal("expected expired remote rejected under verify_dates:true")
	}

	// verify_dates:false accepts the expired remote (chain still validated).
	relaxed, err := p.ClientConfig(writeProfile(t, "dates-relaxed", clientLeaf, root, func(rp *config.ResolvedTLSProfile) {
		rp.VerifyDates = false
	}))
	if err != nil {
		t.Fatalf("ClientConfig relaxed: %v", err)
	}
	if _, _, cliErr := handshake(t, expSrv, relaxed); cliErr != nil {
		t.Fatalf("expected expired remote accepted under verify_dates:false: %v", cliErr)
	}

	// verify_dates:false still rejects an expired cert from an untrusted CA.
	untrustedSrv := &tls.Config{Certificates: []tls.Certificate{peerCert(expiredUntrusted)}}
	if _, _, cliErr := handshake(t, untrustedSrv, relaxed); cliErr == nil {
		t.Fatal("expected untrusted-CA cert rejected even under verify_dates:false")
	}
}

// --- AC8: client validates against the configured CA -------------------------

func TestClientValidatesAgainstCA(t *testing.T) {
	root := ca(t, "root")
	other := ca(t, "untrusted")
	clientLeaf := clientAuth(t, "agent", root)
	inBundle := serverAuth(t, "server", root, "server.test")
	outOfBundle := serverAuth(t, "server", other, "server.test")

	p := NewStdProvider(nil)
	cliCfg, err := p.ClientConfig(writeProfile(t, "ca", clientLeaf, root, nil))
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}

	okSrv := &tls.Config{Certificates: []tls.Certificate{peerCert(inBundle)}}
	if _, _, cliErr := handshake(t, okSrv, withSNI(cliCfg, "server.test")); cliErr != nil {
		t.Fatalf("expected in-bundle remote accepted: %v", cliErr)
	}

	badSrv := &tls.Config{Certificates: []tls.Certificate{peerCert(outOfBundle)}}
	if _, _, cliErr := handshake(t, badSrv, withSNI(cliCfg, "server.test")); cliErr == nil {
		t.Fatal("expected remote signed by an untrusted CA rejected")
	}
}

// --- Negatives ---------------------------------------------------------------

func TestVerifyPeerWithoutCABundleErrors(t *testing.T) {
	root := ca(t, "root")
	server := serverAuth(t, "server", root, "server.test")

	p := NewStdProvider(nil)
	_, err := p.ServerConfig(writeProfile(t, "nobundle", server, nil, func(rp *config.ResolvedTLSProfile) {
		rp.VerifyPeer = true
	}))
	if err == nil || !strings.Contains(err.Error(), "verify_peer requires a ca bundle") {
		t.Fatalf("expected verify_peer-without-ca error, got %v", err)
	}
}

func TestUnknownCipherErrors(t *testing.T) {
	root := ca(t, "root")
	server := serverAuth(t, "server", root, "server.test")

	p := NewStdProvider(nil)
	rp := writeProfile(t, "badcipher", server, nil, func(rp *config.ResolvedTLSProfile) {
		rp.Ciphers = []string{"TLS_NOT_A_REAL_SUITE"}
	})
	if _, err := p.ServerConfig(rp); err == nil || !strings.Contains(err.Error(), "unknown or non-TLS1.2 cipher") {
		t.Fatalf("expected unknown-cipher error from ServerConfig, got %v", err)
	}
	rp.Name = "badcipher-client"
	if _, err := p.ClientConfig(rp); err == nil || !strings.Contains(err.Error(), "unknown or non-TLS1.2 cipher") {
		t.Fatalf("expected unknown-cipher error from ClientConfig, got %v", err)
	}
}

func TestUnsupportedMinVersionErrors(t *testing.T) {
	root := ca(t, "root")
	server := serverAuth(t, "server", root, "server.test")

	p := NewStdProvider(nil)
	rp := writeProfile(t, "badver", server, nil, func(rp *config.ResolvedTLSProfile) {
		rp.MinVersion = config.TLSVersion("tlsv1.1")
	})
	if _, err := p.ServerConfig(rp); err == nil || !strings.Contains(err.Error(), "unsupported min_version") {
		t.Fatalf("expected unsupported-min_version error, got %v", err)
	}
}
