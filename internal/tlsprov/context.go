package tlsprov

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// cached returns the *tls.Config for key from the per-provider cache, building it with
// build (and storing the result) on a miss. The build closure carries the role-specific
// verification wiring; everything common lives in loadAndBase.
func (p *StdProvider) cached(key string, build func() (*tls.Config, error)) (*tls.Config, error) {
	p.mu.Lock()
	if cfg, ok := p.cfgCache[key]; ok {
		p.mu.Unlock()
		return cfg, nil
	}
	p.mu.Unlock()

	cfg, err := build()
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.cfgCache[key] = cfg
	p.mu.Unlock()
	return cfg, nil
}

// loadAndBase loads rp's certificate material and builds the half of a *tls.Config that
// is identical for inbound and outbound use: the presented certificate, the version
// floor, and the TLS 1.2 cipher allowlist. Role-specific peer verification is layered on
// by the caller.
func (p *StdProvider) loadAndBase(rp config.ResolvedTLSProfile) (*tls.Config, *Material, error) {
	m, err := p.Load(rp)
	if err != nil {
		return nil, nil, err
	}
	minVersion, err := mapVersion(rp.MinVersion)
	if err != nil {
		return nil, nil, fmt.Errorf("tls_profiles[%q]: %w", rp.Name, err)
	}
	suites, err := mapCiphers(rp.Ciphers)
	if err != nil {
		return nil, nil, fmt.Errorf("tls_profiles[%q]: %w", rp.Name, err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{m.Certificate},
		MinVersion:   minVersion,
		MaxVersion:   tls.VersionTLS13,
		CipherSuites: suites,
	}, m, nil
}

// ServerConfig builds the inbound (server-side) *tls.Config for rp, enforcing the
// version floor, cipher allowlist, and — when verify_peer is set — mutual-TLS with
// depth, subject, and date policy. Built configs are cached per role+profile.
func (p *StdProvider) ServerConfig(rp config.ResolvedTLSProfile) (*tls.Config, error) {
	return p.cached("server:"+rp.Name, func() (*tls.Config, error) {
		cfg, m, err := p.loadAndBase(rp)
		if err != nil {
			return nil, err
		}
		if rp.VerifyPeer {
			if m.TrustPool == nil {
				return nil, fmt.Errorf("tls_profiles[%q]: verify_peer requires a ca bundle", rp.Name)
			}
			cfg.ClientCAs = m.TrustPool
			if rp.VerifyDates {
				cfg.ClientAuth = tls.RequireAndVerifyClientCert
				cfg.VerifyPeerCertificate = verifier(rp.VerifyDepth, rp.VerifySubjects)
			} else {
				// Go's RequireAndVerifyClientCert always enforces dates and cannot be told
				// otherwise (InsecureSkipVerify is client-only), so require a cert but verify
				// it ourselves with the date window relaxed. A client cert carries no hostname.
				cfg.ClientAuth = tls.RequireAnyClientCert
				cfg.VerifyConnection = connVerifier(m.TrustPool, rp.VerifyDepth, rp.VerifySubjects, x509.ExtKeyUsageClientAuth, false)
			}
		} else {
			cfg.ClientAuth = tls.NoClientCert
		}
		return cfg, nil
	})
}

// ClientConfig builds the outbound (client-side) *tls.Config for rp. By default the
// outbound leg is encrypt-only: it accepts any server certificate (the relaxed posture;
// strict validation is opt-in). Setting verify_peer enables validation of the remote
// server certificate against the configured CA bundle (system roots when none is
// configured), with hostname, depth, and subject checks; verify_dates false then relaxes
// only the date window. Built configs are cached per role+profile.
func (p *StdProvider) ClientConfig(rp config.ResolvedTLSProfile) (*tls.Config, error) {
	return p.cached("client:"+rp.Name, func() (*tls.Config, error) {
		cfg, m, err := p.loadAndBase(rp)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = m.TrustPool
		switch {
		case !rp.VerifyPeer:
			// Relaxed default: encrypt-only. Accept any server certificate — self-signed,
			// expired, hostname mismatch, or signed by an untrusted CA. Operators opt into
			// validation with verify_peer: true.
			cfg.InsecureSkipVerify = true //nolint:gosec // G402: encrypt-only by design; opt in with verify_peer
		case rp.VerifyDates:
			// Strict: Go validates chain, dates, hostname, and key usage; the callback adds
			// the depth and subject checks.
			cfg.VerifyPeerCertificate = verifier(rp.VerifyDepth, rp.VerifySubjects)
		default:
			// verify_peer with relaxed dates: disable Go's built-in (date-enforcing)
			// verification and re-verify the full chain, key usage, AND hostname ourselves.
			cfg.InsecureSkipVerify = true //nolint:gosec // G402: re-verified incl. hostname in connVerifier
			cfg.VerifyConnection = connVerifier(m.TrustPool, rp.VerifyDepth, rp.VerifySubjects, x509.ExtKeyUsageServerAuth, true)
		}
		return cfg, nil
	})
}
