// Package config provides loading and validation of the sequencer YAML configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// FailurePolicy controls what the sequencer does when an application in the chain fails.
type FailurePolicy string

const (
	// FailureSkip skips the failing application and continues the chain.
	FailureSkip FailurePolicy = "skip"
	// FailureAbort aborts the call when the application fails.
	FailureAbort FailurePolicy = "abort"
)

// MediaMode controls whether an application receives a media fork.
type MediaMode string

const (
	// MediaTap forks both call directions to the app as two recvonly RTP streams.
	MediaTap MediaMode = "tap"
	// MediaNone offers the app an inactive audio stream; no RTP is sent.
	MediaNone MediaMode = "none"
)

// LogLevel controls the minimum severity of log messages emitted by the engine.
type LogLevel string

const (
	LogLevelDebug LogLevel = "debug"
	LogLevelInfo  LogLevel = "info"
	LogLevelWarn  LogLevel = "warn"
	LogLevelError LogLevel = "error"
)

// Transport selects the SIP transport for an endpoint (listener, app, or next hop).
type Transport string

const (
	// TransportUDP is the default plain UDP transport.
	TransportUDP Transport = "udp"
	// TransportTCP is plain TCP transport.
	TransportTCP Transport = "tcp"
	// TransportTLS is TLS-over-TCP; it requires a referenced tls_profile.
	TransportTLS Transport = "tls"
)

// TLSVersion is the negotiated minimum TLS protocol version floor.
type TLSVersion string

const (
	// TLSv12 is the TLS 1.2 floor (the secure default).
	TLSv12 TLSVersion = "tlsv1.2"
	// TLSv13 is the TLS 1.3 floor.
	TLSv13 TLSVersion = "tlsv1.3"
)

// SIP holds SIP listener configuration.
type SIP struct {
	Listen string `yaml:"listen"`
}

// TLS holds the optional TLS listener configuration. An empty Listen disables it.
type TLS struct {
	Listen     string              `yaml:"listen"`
	TLSProfile string              `yaml:"tls_profile"`
	Resolved   *ResolvedTLSProfile `yaml:"-"`
}

// WS holds the optional plain WebSocket listener configuration. An empty Listen
// disables it. It carries no tls_profile (plain transport, dev use).
type WS struct {
	Listen string `yaml:"listen"`
}

// WSS holds the optional secure WebSocket listener configuration. It is structurally
// identical to TLS and reuses the same ResolvedTLSProfile model. An empty Listen
// disables it.
type WSS struct {
	Listen     string              `yaml:"listen"`
	TLSProfile string              `yaml:"tls_profile"`
	Resolved   *ResolvedTLSProfile `yaml:"-"`
}

// TLSProfile is a named, reusable certificate + crypto/verification policy as written
// in YAML. VerifyDepth and VerifyDates are pointers so an absent key (apply the secure
// default) is distinguishable from an explicit value.
type TLSProfile struct {
	Cert           string   `yaml:"cert"`
	Key            string   `yaml:"key"`
	Passphrase     string   `yaml:"passphrase"`
	CA             string   `yaml:"ca"`
	MinVersion     string   `yaml:"min_version"`
	Ciphers        []string `yaml:"ciphers"`
	VerifyPeer     bool     `yaml:"verify_peer"`
	VerifyDepth    *int     `yaml:"verify_depth"`
	VerifyDates    *bool    `yaml:"verify_dates"`
	VerifySubjects []string `yaml:"verify_subjects"`
	ConnectTimeout string   `yaml:"connect_timeout"`
}

// ResolvedTLSProfile is the flat, library-agnostic TLS policy with all defaults applied.
// It is the cross-story contract consumed downstream (no crypto/tls types here).
type ResolvedTLSProfile struct {
	Name           string
	Cert           string
	Key            string
	Passphrase     string
	CA             string
	MinVersion     TLSVersion
	Ciphers        []string
	VerifyPeer     bool
	VerifyDepth    int
	VerifyDates    bool
	VerifySubjects []string
	ConnectTimeout time.Duration
}

// RTP holds RTP port range configuration. IdleTimeout is the optional media-inactivity
// window: an established call that exchanges no RTP or RTCP (in either direction) for
// longer than this is torn down, reclaiming its ports and relay goroutines when an
// endpoint or PBX vanishes without a BYE. It is a Go duration string; an empty value
// defaults to 5m and "0" disables the reaper. IdleTimeoutDur is the resolved value.
type RTP struct {
	PortRange      string        `yaml:"port_range"`
	IdleTimeout    string        `yaml:"idle_timeout"`
	IdleTimeoutDur time.Duration `yaml:"-"`
}

// Media holds media-anchoring configuration. PublicAddress is the publicly
// reachable address the secured (WebRTC/ICE-lite) leg advertises as its host
// candidate. An empty value lets the engine fall back to the signaling host.
type Media struct {
	PublicAddress string `yaml:"public_address"`
}

// Observability holds the optional metrics/health HTTP listener configuration.
// An empty Listen disables observability (no HTTP server).
type Observability struct {
	Listen string `yaml:"listen"`
}

// Application describes a single external SIP application in the sequence chain.
// Timeout is the optional per-app setup deadline (dial + answer wait) as a Go duration
// string; an empty value falls back to the global LegTimeout. TimeoutDur is the resolved
// value (zero means "use the global default").
//
// Routing optionally filters which inbound requests this app receives. A nil Routing
// (the default) matches every request; a present block only routes requests whose
// From/To/method satisfy all specified fields. RoutingRe holds the compiled, resolved
// rule used at runtime.
type Application struct {
	Name       string              `yaml:"name"`
	URI        string              `yaml:"uri"`
	OnFailure  FailurePolicy       `yaml:"on_failure"`
	Media      MediaMode           `yaml:"media"`
	Transport  Transport           `yaml:"transport"`
	TLSProfile string              `yaml:"tls_profile"`
	Timeout    string              `yaml:"timeout"`
	Routing    *RoutingRule        `yaml:"routing"`
	TimeoutDur time.Duration       `yaml:"-"`
	Resolved   *ResolvedTLSProfile `yaml:"-"`
	RoutingRe  *ResolvedRouting    `yaml:"-"`
}

// NextHop is the terminating hop, an object with a required uri and optional
// transport/tls_profile. A bare string form is not accepted.
type NextHop struct {
	URI        string              `yaml:"uri"`
	Transport  Transport           `yaml:"transport"`
	TLSProfile string              `yaml:"tls_profile"`
	Resolved   *ResolvedTLSProfile `yaml:"-"`
}

// Config is the validated, in-memory representation of the operator-supplied YAML file.
// LegTimeout is the global default answer/setup timeout for any outbound leg (apps without
// their own timeout, the next hop, mid-call re-INVITE, and REFER) as a Go duration string;
// an empty value defaults to 32s. LegTimeoutDur is the resolved, always-positive value.
type Config struct {
	SIP           SIP                   `yaml:"sip"`
	TLS           TLS                   `yaml:"tls"`
	WS            WS                    `yaml:"ws"`
	WSS           WSS                   `yaml:"wss"`
	NextHop       NextHop               `yaml:"next_hop"`
	RTP           RTP                   `yaml:"rtp"`
	Media         Media                 `yaml:"media"`
	Sequence      []Application         `yaml:"sequence"`
	TLSProfiles   map[string]TLSProfile `yaml:"tls_profiles"`
	LogLevel      LogLevel              `yaml:"log_level"`
	Observability Observability         `yaml:"observability"`
	LegTimeout    string                `yaml:"leg_timeout"`
	LegTimeoutDur time.Duration         `yaml:"-"`
}

// rawConfig mirrors Config but tracks presence of the sequence and next_hop keys via pointers.
type rawConfig struct {
	SIP           SIP                   `yaml:"sip"`
	TLS           TLS                   `yaml:"tls"`
	WS            WS                    `yaml:"ws"`
	WSS           WSS                   `yaml:"wss"`
	NextHop       *NextHop              `yaml:"next_hop"`
	RTP           RTP                   `yaml:"rtp"`
	Media         Media                 `yaml:"media"`
	Sequence      *[]Application        `yaml:"sequence"`
	TLSProfiles   map[string]TLSProfile `yaml:"tls_profiles"`
	LogLevel      LogLevel              `yaml:"log_level"`
	Observability Observability         `yaml:"observability"`
	LegTimeout    string                `yaml:"leg_timeout"`
}

// Parse decodes YAML bytes into a validated Config. source is used only in error messages.
func Parse(data []byte, source string) (Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var raw rawConfig
	if err := dec.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", source, err)
	}

	cfg := Config{
		SIP:           raw.SIP,
		TLS:           raw.TLS,
		WS:            raw.WS,
		WSS:           raw.WSS,
		RTP:           raw.RTP,
		Media:         raw.Media,
		TLSProfiles:   raw.TLSProfiles,
		LogLevel:      raw.LogLevel,
		Observability: raw.Observability,
		LegTimeout:    raw.LegTimeout,
	}
	nextHopPresent := raw.NextHop != nil
	if nextHopPresent {
		cfg.NextHop = *raw.NextHop
	}
	sequencePresent := raw.Sequence != nil
	if sequencePresent {
		cfg.Sequence = *raw.Sequence
	}

	applyDefaults(&cfg)

	if err := validate(cfg, sequencePresent, nextHopPresent); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", source, err)
	}
	if err := resolveTimeouts(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", source, err)
	}
	if err := resolveTLS(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", source, err)
	}
	if err := resolveRouting(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %q: %w", source, err)
	}
	return cfg, nil
}

// Load reads the YAML file at path and returns a validated Config.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	return Parse(data, path)
}

func applyDefaults(cfg *Config) {
	if cfg.LogLevel == "" {
		cfg.LogLevel = LogLevelInfo
	}
	for i := range cfg.Sequence {
		if cfg.Sequence[i].OnFailure == "" {
			cfg.Sequence[i].OnFailure = FailureSkip
		}
		if cfg.Sequence[i].Media == "" {
			cfg.Sequence[i].Media = MediaNone
		}
		if cfg.Sequence[i].Transport == "" {
			cfg.Sequence[i].Transport = TransportUDP
		}
	}
	if cfg.NextHop.Transport == "" {
		cfg.NextHop.Transport = TransportUDP
	}
}

func validate(c Config, sequencePresent, nextHopPresent bool) error {
	if c.SIP.Listen == "" {
		return fmt.Errorf("missing required key %q", "sip.listen")
	}
	if !nextHopPresent {
		return fmt.Errorf("missing required key %q", "next_hop")
	}
	if c.NextHop.URI == "" {
		return fmt.Errorf("next_hop: missing required key %q", "uri")
	}
	if c.RTP.PortRange == "" {
		return fmt.Errorf("missing required key %q", "rtp.port_range")
	}
	if !sequencePresent {
		return fmt.Errorf("missing required key %q", "sequence")
	}
	switch c.LogLevel {
	case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
	default:
		return fmt.Errorf("invalid log_level %q (want \"debug\", \"info\", \"warn\", or \"error\")", c.LogLevel)
	}
	if c.Observability.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Observability.Listen); err != nil {
			return fmt.Errorf("invalid observability.listen %q: %w", c.Observability.Listen, err)
		}
	}
	// media.public_address, when set, is a bare host/IP advertised as the ICE-lite
	// host candidate — it must not carry a port (the secured leg owns its own port).
	if c.Media.PublicAddress != "" {
		if _, _, err := net.SplitHostPort(c.Media.PublicAddress); err == nil {
			return fmt.Errorf("media.public_address %q: want a bare host or IP", c.Media.PublicAddress)
		}
	}
	for i, app := range c.Sequence {
		if app.Name == "" {
			return fmt.Errorf("sequence[%d]: missing required key %q", i, "name")
		}
		if app.URI == "" {
			return fmt.Errorf("sequence[%d] %q: missing required key %q", i, app.Name, "uri")
		}
		if app.OnFailure != FailureSkip && app.OnFailure != FailureAbort {
			return fmt.Errorf("sequence[%d] %q: invalid on_failure %q (want %q or %q)", i, app.Name, app.OnFailure, "skip", "abort")
		}
		if app.Media != MediaTap && app.Media != MediaNone {
			return fmt.Errorf("sequence[%d] %q: invalid media %q (want %q or %q)", i, app.Name, app.Media, "tap", "none")
		}
	}
	if err := validateTLSWiring(c); err != nil {
		return err
	}
	return validateTLSProfiles(c)
}

// validateTLSWiring checks transport enums and tls_profile reference wiring on every
// endpoint: TLS endpoints require a profile, non-TLS endpoints must not carry one (R4),
// and referenced profiles must exist. No certificate files are read here.
func validateTLSWiring(c Config) error {
	for i, app := range c.Sequence {
		switch app.Transport {
		case TransportUDP, TransportTCP, TransportTLS:
		default:
			return fmt.Errorf("sequence[%d] %q: invalid transport %q", i, app.Name, app.Transport)
		}
		if app.Transport == TransportTLS && app.TLSProfile == "" {
			return fmt.Errorf("sequence[%d] %q: transport tls requires a tls_profile", i, app.Name)
		}
		if app.Transport != TransportTLS && app.TLSProfile != "" {
			return fmt.Errorf("sequence[%d] %q: tls_profile set but transport is %q", i, app.Name, app.Transport)
		}
	}

	switch c.NextHop.Transport {
	case TransportUDP, TransportTCP, TransportTLS:
	default:
		return fmt.Errorf("next_hop: invalid transport %q", c.NextHop.Transport)
	}
	if c.NextHop.Transport == TransportTLS && c.NextHop.TLSProfile == "" {
		return errors.New("next_hop: transport tls requires a tls_profile")
	}
	if c.NextHop.Transport != TransportTLS && c.NextHop.TLSProfile != "" {
		return fmt.Errorf("next_hop: tls_profile set but transport is %q", c.NextHop.Transport)
	}

	if c.TLS.Listen != "" {
		if c.TLS.TLSProfile == "" {
			return errors.New("tls.listen requires a tls_profile")
		}
		if _, _, err := net.SplitHostPort(c.TLS.Listen); err != nil {
			return fmt.Errorf("invalid tls.listen %q: %w", c.TLS.Listen, err)
		}
	}

	// WebSocket listeners mirror the tls.listen wiring rules. WS is plain (no
	// profile); WSS requires an existing tls_profile, exactly like tls.listen.
	if c.WS.Listen != "" {
		if _, _, err := net.SplitHostPort(c.WS.Listen); err != nil {
			return fmt.Errorf("invalid ws.listen %q: %w", c.WS.Listen, err)
		}
	}
	if c.WSS.Listen != "" {
		if c.WSS.TLSProfile == "" {
			return errors.New("wss.listen requires a tls_profile")
		}
		if _, _, err := net.SplitHostPort(c.WSS.Listen); err != nil {
			return fmt.Errorf("invalid wss.listen %q: %w", c.WSS.Listen, err)
		}
		if _, ok := c.TLSProfiles[c.WSS.TLSProfile]; !ok {
			return fmt.Errorf("wss.listen: unknown tls_profile %q", c.WSS.TLSProfile)
		}
	}

	// Every referenced profile name must exist in tls_profiles.
	if c.TLS.Listen != "" {
		if _, ok := c.TLSProfiles[c.TLS.TLSProfile]; !ok {
			return fmt.Errorf("tls.listen: unknown tls_profile %q", c.TLS.TLSProfile)
		}
	}
	for i, app := range c.Sequence {
		if app.Transport == TransportTLS {
			if _, ok := c.TLSProfiles[app.TLSProfile]; !ok {
				return fmt.Errorf("sequence[%d] %q: unknown tls_profile %q", i, app.Name, app.TLSProfile)
			}
		}
	}
	if c.NextHop.Transport == TransportTLS {
		if _, ok := c.TLSProfiles[c.NextHop.TLSProfile]; !ok {
			return fmt.Errorf("next_hop: unknown tls_profile %q", c.NextHop.TLSProfile)
		}
	}
	return nil
}

// validateTLSProfiles checks the field syntax of every defined profile (unused profiles
// included). Cipher names are carried opaque here; their validity belongs to the provider.
func validateTLSProfiles(c Config) error {
	for name, p := range c.TLSProfiles {
		if p.Cert == "" || p.Key == "" {
			return fmt.Errorf("tls_profiles[%q]: missing cert/key", name)
		}
		if p.MinVersion != "" && p.MinVersion != string(TLSv12) && p.MinVersion != string(TLSv13) {
			return fmt.Errorf("tls_profiles[%q]: unsupported min_version %q", name, p.MinVersion)
		}
		for _, cipher := range p.Ciphers {
			if cipher == "" {
				return fmt.Errorf("tls_profiles[%q]: empty ciphers entry", name)
			}
		}
		for _, subject := range p.VerifySubjects {
			if subject == "" {
				return fmt.Errorf("tls_profiles[%q]: empty verify_subjects entry", name)
			}
		}
		if p.ConnectTimeout != "" {
			if _, err := time.ParseDuration(p.ConnectTimeout); err != nil {
				return fmt.Errorf("tls_profiles[%q]: invalid connect_timeout %q", name, p.ConnectTimeout)
			}
		}
		if p.VerifyDepth != nil && *p.VerifyDepth < 0 {
			return fmt.Errorf("tls_profiles[%q]: verify_depth must be >= 0", name)
		}
	}
	return nil
}

// defaultLegTimeout is the global answer/setup timeout applied when leg_timeout is omitted.
// It matches the SIP INVITE client transaction Timer B (64*T1).
const defaultLegTimeout = 32 * time.Second

// defaultIdleTimeout is the media-inactivity window applied when rtp.idle_timeout is omitted.
const defaultIdleTimeout = 5 * time.Minute

// resolveTimeouts parses the global leg_timeout and each app's timeout into time.Duration,
// defaulting the global to defaultLegTimeout when omitted and leaving an omitted per-app
// timeout at zero (the "use the global default" sentinel). A present value must parse as a
// Go duration and be strictly positive.
func resolveTimeouts(cfg *Config) error {
	cfg.LegTimeoutDur = defaultLegTimeout
	if cfg.LegTimeout != "" {
		d, err := time.ParseDuration(cfg.LegTimeout)
		if err != nil {
			return fmt.Errorf("invalid leg_timeout %q: %w", cfg.LegTimeout, err)
		}
		if d <= 0 {
			return fmt.Errorf("leg_timeout %q: must be > 0", cfg.LegTimeout)
		}
		cfg.LegTimeoutDur = d
	}
	for i := range cfg.Sequence {
		app := &cfg.Sequence[i]
		if app.Timeout == "" {
			continue
		}
		d, err := time.ParseDuration(app.Timeout)
		if err != nil {
			return fmt.Errorf("sequence[%d] %q: invalid timeout %q: %w", i, app.Name, app.Timeout, err)
		}
		if d <= 0 {
			return fmt.Errorf("sequence[%d] %q: timeout %q must be > 0", i, app.Name, app.Timeout)
		}
		app.TimeoutDur = d
	}
	cfg.RTP.IdleTimeoutDur = defaultIdleTimeout
	if cfg.RTP.IdleTimeout != "" {
		d, err := time.ParseDuration(cfg.RTP.IdleTimeout)
		if err != nil {
			return fmt.Errorf("invalid rtp.idle_timeout %q: %w", cfg.RTP.IdleTimeout, err)
		}
		if d < 0 {
			return fmt.Errorf("rtp.idle_timeout %q: must be >= 0 (0 disables)", cfg.RTP.IdleTimeout)
		}
		cfg.RTP.IdleTimeoutDur = d
	}
	return nil
}

// resolveTLS joins each TLS endpoint to its named profile, applies policy defaults, and
// attaches a shared *ResolvedTLSProfile. Endpoints naming the same profile receive the
// same pointer (literal identity), so downstream stories can dedup by pointer. It runs
// only after validate confirms every referenced profile exists.
func resolveTLS(cfg *Config) error {
	cache := map[string]*ResolvedTLSProfile{}
	resolve := func(name string) (*ResolvedTLSProfile, error) {
		if r, ok := cache[name]; ok {
			return r, nil
		}
		p := cfg.TLSProfiles[name]
		r := &ResolvedTLSProfile{
			Name:           name,
			Cert:           p.Cert,
			Key:            p.Key,
			Passphrase:     p.Passphrase,
			CA:             p.CA,
			MinVersion:     TLSv12,
			Ciphers:        p.Ciphers,
			VerifyPeer:     p.VerifyPeer,
			VerifyDepth:    2,
			VerifyDates:    true,
			VerifySubjects: p.VerifySubjects,
		}
		if p.MinVersion != "" {
			r.MinVersion = TLSVersion(p.MinVersion)
		}
		if p.VerifyDepth != nil {
			r.VerifyDepth = *p.VerifyDepth
		}
		if p.VerifyDates != nil {
			r.VerifyDates = *p.VerifyDates
		}
		if p.ConnectTimeout != "" {
			d, err := time.ParseDuration(p.ConnectTimeout)
			if err != nil {
				return nil, fmt.Errorf("tls_profiles[%q]: invalid connect_timeout %q", name, p.ConnectTimeout)
			}
			r.ConnectTimeout = d
		}
		cache[name] = r
		return r, nil
	}

	if cfg.TLS.Listen != "" {
		r, err := resolve(cfg.TLS.TLSProfile)
		if err != nil {
			return err
		}
		cfg.TLS.Resolved = r
	}
	if cfg.WSS.Listen != "" {
		r, err := resolve(cfg.WSS.TLSProfile)
		if err != nil {
			return err
		}
		cfg.WSS.Resolved = r
	}
	for i := range cfg.Sequence {
		if cfg.Sequence[i].Transport == TransportTLS {
			r, err := resolve(cfg.Sequence[i].TLSProfile)
			if err != nil {
				return err
			}
			cfg.Sequence[i].Resolved = r
		}
	}
	if cfg.NextHop.Transport == TransportTLS {
		r, err := resolve(cfg.NextHop.TLSProfile)
		if err != nil {
			return err
		}
		cfg.NextHop.Resolved = r
	}
	return nil
}

// resolveRouting compiles each application's routing rule into a ResolvedRouting so
// the bridge can match inbound requests without recompiling per call. A nil rule
// yields a nil ResolvedRouting (matches everything); an invalid regex aborts load.
func resolveRouting(cfg *Config) error {
	for i := range cfg.Sequence {
		app := &cfg.Sequence[i]
		re, err := resolveRoutingRule(app.Routing)
		if err != nil {
			return fmt.Errorf("sequence[%d] %q: %w", i, app.Name, err)
		}
		app.RoutingRe = re
	}
	return nil
}
