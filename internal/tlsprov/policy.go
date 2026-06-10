package tlsprov

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// mapVersion maps a resolved min_version to a crypto/tls version constant. An empty
// value yields the secure TLS 1.2 floor; an unrecognized value is an error.
func mapVersion(v config.TLSVersion) (uint16, error) {
	switch v {
	case "":
		return tls.VersionTLS12, nil
	case config.TLSv12:
		return tls.VersionTLS12, nil
	case config.TLSv13:
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("unsupported min_version %q", v)
	}
}

// mapCiphers maps configured cipher names to crypto/tls suite ids, restricted to
// suites usable with TLS 1.2. An empty list returns nil so Go applies its secure
// defaults. An unknown name, or one not valid for TLS 1.2, is an error. Suite order
// is preserved from the configured list.
func mapCiphers(names []string) ([]uint16, error) {
	if len(names) == 0 {
		return nil, nil
	}
	allowed := map[string]uint16{}
	for _, s := range tls.CipherSuites() {
		for _, ver := range s.SupportedVersions {
			if ver == tls.VersionTLS12 {
				allowed[s.Name] = s.ID
				break
			}
		}
	}
	ids := make([]uint16, 0, len(names))
	for _, name := range names {
		id, ok := allowed[name]
		if !ok {
			return nil, fmt.Errorf("unknown or non-TLS1.2 cipher %q", name)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// checkDepth enforces the OpenSSL verify_depth semantics: the number of intermediate
// CAs between the leaf and the trust anchor must not exceed maxIntermediates. The
// chain is ordered leaf … root.
func checkDepth(chain []*x509.Certificate, maxIntermediates int) error {
	intermediates := len(chain) - 2
	if intermediates < 0 {
		intermediates = 0
	}
	if intermediates > maxIntermediates {
		return fmt.Errorf("certificate chain too long: %d intermediates exceeds verify_depth %d", intermediates, maxIntermediates)
	}
	return nil
}

// checkSubject enforces the verify_subjects allowlist. An empty allowlist accepts any
// subject; otherwise the leaf's exact subject string must be listed.
func checkSubject(leaf *x509.Certificate, allow []string) error {
	if len(allow) == 0 {
		return nil
	}
	subject := leaf.Subject.String()
	for _, s := range allow {
		if s == subject {
			return nil
		}
	}
	return fmt.Errorf("peer subject %q not in verify_subjects", subject)
}
