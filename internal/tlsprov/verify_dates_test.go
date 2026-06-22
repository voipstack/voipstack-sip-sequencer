package tlsprov

import (
	"crypto/tls"
	"crypto/x509"
	"testing"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// The relaxed default: an outbound TLS leg with verify_peer unset (false) is
// encrypt-only — it accepts ANY server certificate (self-signed, expired, wrong
// hostname, untrusted CA). Strict validation is opt-in via verify_peer.
func TestRelaxedDefaultAcceptsAnyServerCert(t *testing.T) {
	root := ca(t, "root")
	clientLeaf := clientAuth(t, "agent", root)

	p := NewStdProvider(nil)
	// Default profile: verify_peer is unset (false) -> encrypt-only.
	relaxed, err := p.ClientConfig(writeProfile(t, "relaxed-default", clientLeaf, root, nil))
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}

	// A self-signed, expired cert for an unrelated hostname — untrusted on every axis.
	rogue := mint(t, "rogue", nil, false, pastExpiry,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{"rogue.invalid"})
	rogueSrv := &tls.Config{Certificates: []tls.Certificate{peerCert(rogue)}}

	if _, _, cliErr := handshake(t, rogueSrv, withSNI(relaxed, "server.test")); cliErr != nil {
		t.Fatalf("relaxed default must accept any server cert (encrypt-only): %v", cliErr)
	}
}

// With verify_peer enabled, verify_dates:false relaxes ONLY the date window — it must
// not open a hostname (machine-in-the-middle) bypass. A CA-valid cert for the wrong host
// is rejected; a CA-valid expired cert for the right host is accepted.
func TestVerifyDatesFalseStillChecksHostname(t *testing.T) {
	root := ca(t, "root")
	clientLeaf := clientAuth(t, "agent", root)
	wrongHost := mint(t, "attacker", root, false, pastExpiry,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{"attacker.test"})
	rightHost := mint(t, "server", root, false, pastExpiry,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []string{"server.test"})

	p := NewStdProvider(nil)
	relaxed, err := p.ClientConfig(writeProfile(t, "verify-relaxed-host", clientLeaf, root, func(rp *config.ResolvedTLSProfile) {
		rp.VerifyPeer = true
		rp.VerifyDates = false
	}))
	if err != nil {
		t.Fatalf("ClientConfig: %v", err)
	}

	// Wrong-hostname cert rejected even though dates are relaxed and the CA is trusted.
	wrongSrv := &tls.Config{Certificates: []tls.Certificate{peerCert(wrongHost)}}
	if _, _, cliErr := handshake(t, wrongSrv, withSNI(relaxed, "server.test")); cliErr == nil {
		t.Fatal("expected wrong-hostname cert rejected under verify_peer + verify_dates:false (MITM guard)")
	}

	// Correct-hostname expired cert accepted (dates relaxed, identity preserved).
	rightSrv := &tls.Config{Certificates: []tls.Certificate{peerCert(rightHost)}}
	if _, _, cliErr := handshake(t, rightSrv, withSNI(relaxed, "server.test")); cliErr != nil {
		t.Fatalf("expected correct-hostname expired cert accepted under verify_dates:false: %v", cliErr)
	}
}

// verify_dates:false on an inbound mTLS profile actually relaxes the date window on the
// client certificate (and still enforces the chain). Before the fix the server set
// InsecureSkipVerify (a client-only no-op), so Go's RequireAndVerifyClientCert still
// rejected expired client certs — silently ignoring verify_dates:false.
func TestServerVerifyDatesFalseAcceptsExpiredClientCert(t *testing.T) {
	root := ca(t, "root")
	other := ca(t, "untrusted")
	server := serverAuth(t, "server", root, "server.test")
	expiredClient := mint(t, "agent", root, false, pastExpiry,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	expiredUntrusted := mint(t, "agent", other, false, pastExpiry,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)

	p := NewStdProvider(nil)
	srvCfg, err := p.ServerConfig(writeProfile(t, "mtls-relaxed", server, root, func(rp *config.ResolvedTLSProfile) {
		rp.VerifyPeer = true
		rp.VerifyDates = false
	}))
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}

	// Expired but CA-trusted client cert is accepted.
	okClient := &tls.Config{Certificates: []tls.Certificate{peerCert(expiredClient)}, RootCAs: pool(root), ServerName: "server.test"}
	if _, srvErr, cliErr := handshake(t, srvCfg, okClient); srvErr != nil || cliErr != nil {
		t.Fatalf("expected expired CA-trusted client accepted under verify_dates:false: srv=%v cli=%v", srvErr, cliErr)
	}

	// Expired AND untrusted-CA client cert is still rejected (chain still enforced).
	badClient := &tls.Config{Certificates: []tls.Certificate{peerCert(expiredUntrusted)}, RootCAs: pool(root), ServerName: "server.test"}
	if _, srvErr, _ := handshake(t, srvCfg, badClient); srvErr == nil {
		t.Fatal("expected untrusted-CA client rejected even under verify_dates:false")
	}
}
