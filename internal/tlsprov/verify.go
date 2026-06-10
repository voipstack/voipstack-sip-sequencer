package tlsprov

import (
	"crypto/x509"
	"fmt"
)

// verifier returns a tls.Config.VerifyPeerCertificate callback that enforces the
// depth cap and subject allowlist on the accepted chain, plus optional date
// relaxation.
//
// Strict (checkDates == true) is the secure default: Go has already performed full
// chain, date, and key-usage verification and passes verifiedChains; the callback
// only adds the depth and subject checks. Go's vetted verification is never bypassed.
//
// Relaxed (checkDates == false) is reached only when the caller set
// InsecureSkipVerify, so verifiedChains is nil. The callback rebuilds the chain from
// rawCerts and verifies it against roots with CurrentTime pinned to the leaf's
// NotBefore — neutralizing the date window while keeping full chain and key-usage
// validation — then applies the same depth and subject checks.
func verifier(roots *x509.CertPool, maxIntermediates int, subjects []string, checkDates bool, ku x509.ExtKeyUsage) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		chains := verifiedChains
		if !checkDates {
			certs := make([]*x509.Certificate, 0, len(rawCerts))
			for _, raw := range rawCerts {
				c, err := x509.ParseCertificate(raw)
				if err != nil {
					return fmt.Errorf("parse peer certificate: %w", err)
				}
				certs = append(certs, c)
			}
			if len(certs) == 0 {
				return fmt.Errorf("no peer certificate presented")
			}
			leaf := certs[0]
			interPool := x509.NewCertPool()
			for _, c := range certs[1:] {
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
			built, err := leaf.Verify(x509.VerifyOptions{
				Roots:         vroots,
				Intermediates: interPool,
				KeyUsages:     []x509.ExtKeyUsage{ku},
				CurrentTime:   leaf.NotBefore,
			})
			if err != nil {
				return fmt.Errorf("certificate chain verification failed: %w", err)
			}
			chains = built
		}

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
}
