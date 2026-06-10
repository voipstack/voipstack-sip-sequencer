package tlsprov

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// ServerConfig builds the inbound (server-side) *tls.Config for rp, enforcing the
// version floor, cipher allowlist, and — when verify_peer is set — mutual-TLS with
// depth, subject, and date policy. Built configs are cached per role+profile.
func (p *StdProvider) ServerConfig(rp config.ResolvedTLSProfile) (*tls.Config, error) {
	key := "server:" + rp.Name

	p.mu.Lock()
	if cfg, ok := p.cfgCache[key]; ok {
		p.mu.Unlock()
		return cfg, nil
	}
	p.mu.Unlock()

	m, err := p.Load(rp)
	if err != nil {
		return nil, err
	}
	minVersion, err := mapVersion(rp.MinVersion)
	if err != nil {
		return nil, fmt.Errorf("tls_profiles[%q]: %w", rp.Name, err)
	}
	suites, err := mapCiphers(rp.Ciphers)
	if err != nil {
		return nil, fmt.Errorf("tls_profiles[%q]: %w", rp.Name, err)
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{m.Certificate},
		MinVersion:   minVersion,
		MaxVersion:   tls.VersionTLS13,
		CipherSuites: suites,
	}

	if rp.VerifyPeer {
		if m.TrustPool == nil {
			return nil, fmt.Errorf("tls_profiles[%q]: verify_peer requires a ca bundle", rp.Name)
		}
		cfg.ClientCAs = m.TrustPool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		if !rp.VerifyDates {
			cfg.InsecureSkipVerify = true
		}
		cfg.VerifyPeerCertificate = verifier(m.TrustPool, rp.VerifyDepth, rp.VerifySubjects, rp.VerifyDates, x509.ExtKeyUsageClientAuth)
	} else {
		cfg.ClientAuth = tls.NoClientCert
	}

	p.mu.Lock()
	p.cfgCache[key] = cfg
	p.mu.Unlock()
	return cfg, nil
}

// ClientConfig builds the outbound (client-side) *tls.Config for rp. The client
// always validates the remote server certificate against the configured CA bundle
// (system roots when none is configured), with depth and subject checks; verify_dates
// false relaxes only the date window. Built configs are cached per role+profile.
func (p *StdProvider) ClientConfig(rp config.ResolvedTLSProfile) (*tls.Config, error) {
	key := "client:" + rp.Name

	p.mu.Lock()
	if cfg, ok := p.cfgCache[key]; ok {
		p.mu.Unlock()
		return cfg, nil
	}
	p.mu.Unlock()

	m, err := p.Load(rp)
	if err != nil {
		return nil, err
	}
	minVersion, err := mapVersion(rp.MinVersion)
	if err != nil {
		return nil, fmt.Errorf("tls_profiles[%q]: %w", rp.Name, err)
	}
	suites, err := mapCiphers(rp.Ciphers)
	if err != nil {
		return nil, fmt.Errorf("tls_profiles[%q]: %w", rp.Name, err)
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{m.Certificate},
		MinVersion:   minVersion,
		MaxVersion:   tls.VersionTLS13,
		CipherSuites: suites,
		RootCAs:      m.TrustPool,
	}
	if !rp.VerifyDates {
		cfg.InsecureSkipVerify = true
	}
	cfg.VerifyPeerCertificate = verifier(m.TrustPool, rp.VerifyDepth, rp.VerifySubjects, rp.VerifyDates, x509.ExtKeyUsageServerAuth)

	p.mu.Lock()
	p.cfgCache[key] = cfg
	p.mu.Unlock()
	return cfg, nil
}
