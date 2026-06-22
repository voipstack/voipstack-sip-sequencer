package tlsprov

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
)

// verifier is the strict-path VerifyPeerCertificate callback. Go has already performed
// full chain, date, key-usage, and (client) hostname verification and passes
// verifiedChains; this callback only adds the depth cap and subject allowlist on top.
// Go's vetted verification is never bypassed.
func verifier(maxIntermediates int, subjects []string) func([][]byte, [][]*x509.Certificate) error {
	return func(_ [][]byte, verifiedChains [][]*x509.Certificate) error {
		return checkChains(verifiedChains, maxIntermediates, subjects)
	}
}

// connVerifier is the relaxed-dates VerifyConnection callback. Go's built-in
// verification is off (InsecureSkipVerify on the client, RequireAnyClientCert on the
// server), so this rebuilds and verifies the full chain from the presented certs —
// pinning CurrentTime to the leaf's NotBefore to neutralize ONLY the date window —
// while still enforcing the trust roots, the key usage, and (when checkHostname) the
// peer hostname carried in cs.ServerName. Relaxing dates must never relax identity.
// VerifyConnection is used rather than VerifyPeerCertificate because only it carries the
// negotiated ServerName needed for the hostname check.
func connVerifier(roots *x509.CertPool, maxIntermediates int, subjects []string, ku x509.ExtKeyUsage, checkHostname bool) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("no peer certificate presented")
		}
		leaf := cs.PeerCertificates[0]
		interPool := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			interPool.AddCert(c)
		}
		vroots := roots
		if vroots == nil {
			sys, err := x509.SystemCertPool()
			if err != nil {
				return fmt.Errorf("load system roots: %w", err)
			}
			vroots = sys
		}
		opts := x509.VerifyOptions{
			Roots:         vroots,
			Intermediates: interPool,
			KeyUsages:     []x509.ExtKeyUsage{ku},
			CurrentTime:   leaf.NotBefore, // relax only the date window
		}
		if checkHostname {
			opts.DNSName = cs.ServerName // relaxing dates must not relax identity
		}
		built, err := leaf.Verify(opts)
		if err != nil {
			return fmt.Errorf("certificate chain verification failed: %w", err)
		}
		return checkChains(built, maxIntermediates, subjects)
	}
}

// checkChains accepts the peer when any already-verified chain passes both the depth cap
// and the subject allowlist; otherwise it returns the first failure (or a generic error
// when no chain was supplied).
func checkChains(chains [][]*x509.Certificate, maxIntermediates int, subjects []string) error {
	var firstErr error
	for _, chain := range chains {
		if len(chain) == 0 {
			continue
		}
		if err := checkDepth(chain, maxIntermediates); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := checkSubject(chain[0], subjects); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return nil
	}
	if firstErr != nil {
		return firstErr
	}
	return fmt.Errorf("no verified certificate chain")
}
