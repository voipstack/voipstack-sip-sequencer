package tlsprov

import "github.com/voipstack/voipstack-sip-sequencer/internal/config"

// LoadAll eagerly loads every resolved tls_profile referenced by cfg — the inbound
// TLS listener, each sequence application, and the next hop — so an unloadable
// certificate aborts startup before any listener binds. It returns the first load
// error (already audit-logged). A config with no TLS profiles is a no-op.
//
// Deduplication is handled by the provider cache, so passing the same profile/path
// more than once is safe.
func LoadAll(cfg config.Config, p Provider) error {
	var profiles []*config.ResolvedTLSProfile
	profiles = append(profiles, cfg.TLS.Resolved)
	for i := range cfg.Sequence {
		profiles = append(profiles, cfg.Sequence[i].Resolved)
	}
	profiles = append(profiles, cfg.NextHop.Resolved)

	for _, rp := range profiles {
		if rp == nil {
			continue
		}
		if _, err := p.Load(*rp); err != nil {
			return err
		}
	}
	return nil
}
