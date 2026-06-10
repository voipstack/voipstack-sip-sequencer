// Package tlsprov is the single swappable TLS boundary. It is the only package
// permitted to import crypto/tls and crypto/x509, isolating the library from the
// listener, dialers, and config parsing. This story implements its first
// responsibility: loading a resolved tls_profile's certificate material.
package tlsprov

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// Material is the loaded artifact for one tls_profile: the parsed key pair plus an
// optional trust pool. It is the std-provider's concrete type, consumed downstream
// (STORY-001-014) to build a *tls.Config. The swappable seam is Provider, not Material.
type Material struct {
	Certificate tls.Certificate
	TrustPool   *x509.CertPool
}

// Provider is the consumer-facing TLS boundary. An alternate implementation
// (OpenSSL/HSM) satisfies the same interface without changing callers.
type Provider interface {
	Load(rp config.ResolvedTLSProfile) (*Material, error)
}

// StdProvider is the default Provider over the Go standard library. It caches
// loaded Material keyed by the cert+key path pair so a certificate referenced by
// several endpoints is read from disk exactly once.
type StdProvider struct {
	mu    sync.Mutex
	cache map[string]*Material
	log   *slog.Logger
}

// NewStdProvider constructs a StdProvider. A nil log defaults to slog.Default().
func NewStdProvider(log *slog.Logger) *StdProvider {
	if log == nil {
		log = slog.Default()
	}
	return &StdProvider{
		cache: map[string]*Material{},
		log:   log,
	}
}

// Load returns the certificate Material for rp, reading and caching it on first
// use. Repeated calls for the same cert+key path pair return the cached *Material.
// No certificate, private-key, or passphrase bytes appear in any returned error.
func (p *StdProvider) Load(rp config.ResolvedTLSProfile) (*Material, error) {
	cacheKey := filepath.Clean(rp.Cert) + "\x00" + filepath.Clean(rp.Key)

	p.mu.Lock()
	defer p.mu.Unlock()

	if m, ok := p.cache[cacheKey]; ok {
		return m, nil
	}

	certPEM, err := os.ReadFile(rp.Cert)
	if err != nil {
		p.auditFail(rp, rp.Cert, err)
		return nil, fmt.Errorf("tls_profiles[%q]: read certificate %q: %w", rp.Name, rp.Cert, err)
	}
	keyPEM, err := os.ReadFile(rp.Key)
	if err != nil {
		p.auditFail(rp, rp.Key, err)
		return nil, fmt.Errorf("tls_profiles[%q]: read private key %q: %w", rp.Name, rp.Key, err)
	}

	var cert tls.Certificate
	if rp.Passphrase == "" {
		cert, err = tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			p.auditFail(rp, rp.Key, err)
			return nil, fmt.Errorf("tls_profiles[%q]: invalid certificate/key pair: %w", rp.Name, err)
		}
	} else {
		decPEM, derr := decryptKeyPEM(p.log, keyPEM, rp.Passphrase)
		if derr != nil {
			p.auditFail(rp, rp.Key, derr)
			return nil, fmt.Errorf("tls_profiles[%q]: %w", rp.Name, derr)
		}
		cert, err = tls.X509KeyPair(certPEM, decPEM)
		if err != nil {
			p.auditFail(rp, rp.Key, err)
			return nil, fmt.Errorf("tls_profiles[%q]: invalid certificate/key pair: %w", rp.Name, err)
		}
	}

	var pool *x509.CertPool
	if rp.CA != "" {
		caPEM, err := os.ReadFile(rp.CA)
		if err != nil {
			p.auditFail(rp, rp.CA, err)
			return nil, fmt.Errorf("tls_profiles[%q]: read CA bundle %q: %w", rp.Name, rp.CA, err)
		}
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			p.auditFail(rp, rp.CA, errors.New("no valid CA certificates"))
			return nil, fmt.Errorf("tls_profiles[%q]: no valid CA certificates in %q", rp.Name, rp.CA)
		}
	}

	m := &Material{Certificate: cert, TrustPool: pool}
	p.cache[cacheKey] = m
	return m, nil
}

// auditFail emits a single security-audit line for a failed load. It logs only the
// profile name and the offending path — never the wrapped error, which may carry
// file bytes. The sanitized error is returned to the caller, not logged here.
func (p *StdProvider) auditFail(rp config.ResolvedTLSProfile, path string, _ error) {
	p.log.Error("tls certificate load failed", "profile", rp.Name, "path", path)
}
