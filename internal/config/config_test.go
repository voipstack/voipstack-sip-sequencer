package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

const completeYAML = `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence:
  - name: transcription
    uri: "sip:transcription.example.com"
    on_failure: skip
  - name: recording
    uri: "sip:recording.example.com"
    on_failure: abort
`

func TestParseLoadsCompleteConfigPreservingOrder(t *testing.T) {
	// Given: a complete, well-formed YAML with two sequence entries
	// When: Parse is called
	cfg, err := config.Parse([]byte(completeYAML), "test.yaml")

	// Then: no error and fields match, sequence order preserved
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SIP.Listen != "0.0.0.0:5060" {
		t.Errorf("sip.listen = %q, want %q", cfg.SIP.Listen, "0.0.0.0:5060")
	}
	if cfg.NextHop.URI != "sip:proxy.example.com" {
		t.Errorf("next_hop = %q, want %q", cfg.NextHop.URI, "sip:proxy.example.com")
	}
	if cfg.RTP.PortRange != "10000-20000" {
		t.Errorf("rtp.port_range = %q, want %q", cfg.RTP.PortRange, "10000-20000")
	}
	if len(cfg.Sequence) != 2 {
		t.Fatalf("len(sequence) = %d, want 2", len(cfg.Sequence))
	}
	if cfg.Sequence[0].Name != "transcription" {
		t.Errorf("sequence[0].name = %q, want %q", cfg.Sequence[0].Name, "transcription")
	}
	if cfg.Sequence[1].Name != "recording" {
		t.Errorf("sequence[1].name = %q, want %q", cfg.Sequence[1].Name, "recording")
	}
	if cfg.Sequence[1].OnFailure != config.FailureAbort {
		t.Errorf("sequence[1].on_failure = %q, want %q", cfg.Sequence[1].OnFailure, config.FailureAbort)
	}
}

func TestParseDefaultsOmittedOnFailureToSkip(t *testing.T) {
	// Given: YAML with an application entry that omits on_failure
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence:
  - name: app1
    uri: "sip:app1.example.com"
`
	// When: Parse is called
	cfg, err := config.Parse([]byte(yaml), "test.yaml")

	// Then: on_failure defaults to skip
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Sequence[0].OnFailure != config.FailureSkip {
		t.Errorf("on_failure = %q, want %q", cfg.Sequence[0].OnFailure, config.FailureSkip)
	}
}

func TestParseFailsWhenRequiredKeyMissing(t *testing.T) {
	// Given: YAML missing each required key in turn
	// When: Parse is called
	// Then: error names the missing key

	baseYAML := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence: []
`
	missingCases := []struct {
		name    string
		yaml    string
		wantKey string
	}{
		{
			name: "missing sip.listen",
			yaml: `
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence: []
`,
			wantKey: "sip.listen",
		},
		{
			name: "empty sip.listen",
			yaml: `
sip:
  listen: ""
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence: []
`,
			wantKey: "sip.listen",
		},
		{
			name: "missing next_hop",
			yaml: `
sip:
  listen: "0.0.0.0:5060"
rtp:
  port_range: "10000-20000"
sequence: []
`,
			wantKey: "next_hop",
		},
		{
			name: "missing rtp.port_range",
			yaml: `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp: {}
sequence: []
`,
			wantKey: "rtp.port_range",
		},
		{
			name: "missing sequence key",
			yaml: `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
`,
			wantKey: "sequence",
		},
	}

	_ = baseYAML
	for _, tc := range missingCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tc.yaml), "x.yaml")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("error %q does not contain key %q", err.Error(), tc.wantKey)
			}
		})
	}
}

func TestLoadFailsWhenFileMissingNamingPath(t *testing.T) {
	// Given: a path that does not exist
	path := "/nonexistent/path/config.yaml"

	// When: Load is called
	_, err := config.Load(path)

	// Then: error names the path
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not contain path %q", err.Error(), path)
	}
}

func TestParseFailsOnUnparseableYAML(t *testing.T) {
	// Given: malformed YAML bytes
	bad := []byte(`{broken: yaml: [`)

	// When: Parse is called
	_, err := config.Parse(bad, "bad.yaml")

	// Then: error references the source file
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("error %q does not contain source name", err.Error())
	}
}

func TestParseIgnoresEnvironment(t *testing.T) {
	// Given: NEXT_HOP env var set
	t.Setenv("NEXT_HOP", "sip:env-injected.example.com")

	// When: Parse is called with YAML that has next_hop
	cfg, err := config.Parse([]byte(completeYAML), "test.yaml")

	// Then: cfg.NextHop comes from YAML, not env
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.NextHop.URI == "sip:env-injected.example.com" {
		t.Error("next_hop was read from environment variable — must come only from YAML")
	}
	if cfg.NextHop.URI != "sip:proxy.example.com" {
		t.Errorf("next_hop = %q, want %q", cfg.NextHop.URI, "sip:proxy.example.com")
	}
}

func TestParseFailsOnUnknownKey(t *testing.T) {
	// Given: YAML with a typo key (next_hopp)
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hopp: "typo"
rtp:
  port_range: "10000-20000"
sequence: []
`
	// When: Parse is called
	_, err := config.Parse([]byte(yaml), "typo.yaml")

	// Then: error is returned (strict decoding rejects unknown keys)
	if err == nil {
		t.Fatal("expected error for unknown key, got nil")
	}
}

func TestParseFailsOnInvalidOnFailure(t *testing.T) {
	// Given: YAML with an invalid on_failure value
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence:
  - name: app1
    uri: "sip:app1.example.com"
    on_failure: pause
`
	// When: Parse is called
	_, err := config.Parse([]byte(yaml), "bad.yaml")

	// Then: error names the invalid value
	if err == nil {
		t.Fatal("expected error for invalid on_failure, got nil")
	}
	if !strings.Contains(err.Error(), "pause") {
		t.Errorf("error %q does not mention the bad value %q", err.Error(), "pause")
	}
}

func TestParseAllowsEmptySequence(t *testing.T) {
	// Given: YAML with sequence present but empty
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence: []
`
	// When: Parse is called
	cfg, err := config.Parse([]byte(yaml), "empty.yaml")

	// Then: no error and sequence is empty
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Sequence) != 0 {
		t.Errorf("len(sequence) = %d, want 0", len(cfg.Sequence))
	}
}

func TestParseFailsOnEntryMissingNameOrURI(t *testing.T) {
	// Given: YAML sequence entries missing name or uri
	cases := []struct {
		name    string
		yaml    string
		wantKey string
	}{
		{
			name: "missing name",
			yaml: `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence:
  - uri: "sip:app1.example.com"
`,
			wantKey: "name",
		},
		{
			name: "missing uri",
			yaml: `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence:
  - name: app1
`,
			wantKey: "uri",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tc.yaml), "entry.yaml")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("error %q does not contain key %q", err.Error(), tc.wantKey)
			}
		})
	}
}

// ── MediaMode tests ───────────────────────────────────────────────────────────

// Given: app omits media; When: Parse; Then: media defaults to none (AC5).
func TestParseDefaultsOmittedMediaToNone(t *testing.T) {
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence:
  - name: app1
    uri: "sip:app1.example.com"
`
	cfg, err := config.Parse([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Sequence[0].Media != config.MediaNone {
		t.Errorf("media = %q, want %q", cfg.Sequence[0].Media, config.MediaNone)
	}
}

// Given: app sets media: tap; When: Parse; Then: media == tap.
func TestParseAcceptsMediaTap(t *testing.T) {
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence:
  - name: app1
    uri: "sip:app1.example.com"
    media: tap
`
	cfg, err := config.Parse([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Sequence[0].Media != config.MediaTap {
		t.Errorf("media = %q, want %q", cfg.Sequence[0].Media, config.MediaTap)
	}
}

// Given: app sets media: invalid; When: Parse; Then: error naming the bad value.
func TestParseFailsOnInvalidMedia(t *testing.T) {
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence:
  - name: app1
    uri: "sip:app1.example.com"
    media: stream
`
	_, err := config.Parse([]byte(yaml), "test.yaml")
	if err == nil {
		t.Fatal("expected error for invalid media, got nil")
	}
	if !strings.Contains(err.Error(), "stream") {
		t.Errorf("error %q does not mention bad value %q", err.Error(), "stream")
	}
}

// Given: existing config without media key; When: Parse; Then: loads and defaults to none.
func TestParseBackwardCompatWithoutMediaKey(t *testing.T) {
	cfg, err := config.Parse([]byte(completeYAML), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, app := range cfg.Sequence {
		if app.Media != config.MediaNone {
			t.Errorf("sequence[%d].media = %q, want %q", i, app.Media, config.MediaNone)
		}
	}
}

// ── log_level tests ───────────────────────────────────────────────────────────

// Given YAML with log_level: warn; When Parse; Then cfg.LogLevel == LogLevelWarn.
func TestParseAcceptsExplicitLogLevel(t *testing.T) {
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence: []
log_level: warn
`
	cfg, err := config.Parse([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogLevel != config.LogLevelWarn {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, config.LogLevelWarn)
	}
}

// Given YAML without log_level; When Parse; Then cfg.LogLevel == LogLevelInfo.
func TestParseDefaultsOmittedLogLevelToInfo(t *testing.T) {
	cfg, err := config.Parse([]byte(completeYAML), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LogLevel != config.LogLevelInfo {
		t.Errorf("log_level = %q, want %q", cfg.LogLevel, config.LogLevelInfo)
	}
}

// Given log_level: verbose; When Parse; Then error containing "verbose".
func TestParseFailsOnInvalidLogLevel(t *testing.T) {
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence: []
log_level: verbose
`
	_, err := config.Parse([]byte(yaml), "test.yaml")
	if err == nil {
		t.Fatal("expected error for invalid log_level, got nil")
	}
	if !strings.Contains(err.Error(), "verbose") {
		t.Errorf("error %q does not mention bad value %q", err.Error(), "verbose")
	}
}

func TestNoTLSKeysParsesPlain(t *testing.T) {
	// Given: a complete config with an object next_hop and no TLS keys at all
	// When: Parse is called
	cfg, err := config.Parse([]byte(completeYAML), "test.yaml")

	// Then: it succeeds, no resolved profiles, transports default to udp
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TLS.Resolved != nil {
		t.Error("tls.Resolved should be nil when no TLS configured")
	}
	if cfg.NextHop.Resolved != nil {
		t.Error("next_hop.Resolved should be nil when no TLS configured")
	}
	if cfg.NextHop.Transport != config.TransportUDP {
		t.Errorf("next_hop.transport = %q, want %q", cfg.NextHop.Transport, config.TransportUDP)
	}
	for i, app := range cfg.Sequence {
		if app.Transport != config.TransportUDP {
			t.Errorf("sequence[%d].transport = %q, want %q", i, app.Transport, config.TransportUDP)
		}
		if app.Resolved != nil {
			t.Errorf("sequence[%d].Resolved should be nil when no TLS configured", i)
		}
	}
}

func TestProfileReusedSharesResolvedPointer(t *testing.T) {
	// Given: an app and the next_hop both referencing the same tls_profile
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
  transport: tls
  tls_profile: outbound
rtp:
  port_range: "10000-20000"
tls_profiles:
  outbound:
    cert: /etc/certs/out.pem
    key: /etc/certs/out.key
sequence:
  - name: app
    uri: "sip:app.example.com"
    transport: tls
    tls_profile: outbound
`
	// When: Parse is called
	cfg, err := config.Parse([]byte(yaml), "test.yaml")

	// Then: both endpoints share the exact same *ResolvedTLSProfile pointer
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Sequence[0].Resolved == nil || cfg.NextHop.Resolved == nil {
		t.Fatal("expected both endpoints resolved")
	}
	if cfg.Sequence[0].Resolved != cfg.NextHop.Resolved {
		t.Error("endpoints naming the same profile must share one *ResolvedTLSProfile")
	}
	if cfg.NextHop.Resolved.Cert != "/etc/certs/out.pem" {
		t.Errorf("resolved cert = %q, want %q", cfg.NextHop.Resolved.Cert, "/etc/certs/out.pem")
	}
}

func TestTransportTLSWithoutProfileFails(t *testing.T) {
	// Given: a sequence app with transport tls but no tls_profile
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence:
  - name: app
    uri: "sip:app.example.com"
    transport: tls
`
	// When/Then: parse fails naming the endpoint
	_, err := config.Parse([]byte(yaml), "test.yaml")
	if err == nil {
		t.Fatal("expected error for tls transport without profile")
	}
	if !strings.Contains(err.Error(), "transport tls requires a tls_profile") {
		t.Errorf("error %q missing expected reason", err.Error())
	}
}

func TestTLSListenWithoutProfileFails(t *testing.T) {
	// Given: a tls.listen block with no tls_profile
	yaml := `
sip:
  listen: "0.0.0.0:5060"
tls:
  listen: "0.0.0.0:5061"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence: []
`
	// When/Then: parse fails
	_, err := config.Parse([]byte(yaml), "test.yaml")
	if err == nil {
		t.Fatal("expected error for tls.listen without profile")
	}
	if !strings.Contains(err.Error(), "tls.listen requires a tls_profile") {
		t.Errorf("error %q missing expected reason", err.Error())
	}
}

func TestUnknownProfileFails(t *testing.T) {
	// Given: a tls.listen referencing a profile that does not exist
	yaml := `
sip:
  listen: "0.0.0.0:5060"
tls:
  listen: "0.0.0.0:5061"
  tls_profile: missing
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence: []
`
	// When/Then: parse fails naming the unknown profile
	_, err := config.Parse([]byte(yaml), "test.yaml")
	if err == nil {
		t.Fatal("expected error for unknown profile")
	}
	if !strings.Contains(err.Error(), "unknown tls_profile") || !strings.Contains(err.Error(), "missing") {
		t.Errorf("error %q missing endpoint/profile name", err.Error())
	}
}

func TestTLSAndPlainListenersCoexist(t *testing.T) {
	// Given: both sip.listen and tls.listen set
	yaml := `
sip:
  listen: "0.0.0.0:5060"
tls:
  listen: "0.0.0.0:5061"
  tls_profile: inbound
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
tls_profiles:
  inbound:
    cert: /etc/certs/in.pem
    key: /etc/certs/in.key
sequence: []
`
	// When: Parse is called
	cfg, err := config.Parse([]byte(yaml), "test.yaml")

	// Then: both listeners present and tls resolved
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SIP.Listen != "0.0.0.0:5060" {
		t.Errorf("sip.listen = %q", cfg.SIP.Listen)
	}
	if cfg.TLS.Listen != "0.0.0.0:5061" {
		t.Errorf("tls.listen = %q", cfg.TLS.Listen)
	}
	if cfg.TLS.Resolved == nil {
		t.Error("tls listener should resolve a profile")
	}
}

func TestOmittedPolicyDefaults(t *testing.T) {
	// Given: a profile with only cert/key, all policy omitted
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
  transport: tls
  tls_profile: outbound
rtp:
  port_range: "10000-20000"
tls_profiles:
  outbound:
    cert: /etc/certs/out.pem
    key: /etc/certs/out.key
sequence: []
`
	// When: Parse is called
	cfg, err := config.Parse([]byte(yaml), "test.yaml")

	// Then: secure defaults applied to the resolved profile
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := cfg.NextHop.Resolved
	if r == nil {
		t.Fatal("expected resolved profile")
	}
	if r.MinVersion != config.TLSv12 {
		t.Errorf("min_version = %q, want %q", r.MinVersion, config.TLSv12)
	}
	if r.VerifyPeer {
		t.Error("verify_peer default should be false")
	}
	if r.VerifyDepth != 2 {
		t.Errorf("verify_depth = %d, want 2", r.VerifyDepth)
	}
	if !r.VerifyDates {
		t.Error("verify_dates default should be true")
	}
	if r.VerifySubjects != nil {
		t.Errorf("verify_subjects default should be nil, got %v", r.VerifySubjects)
	}
	if r.ConnectTimeout != 0 {
		t.Errorf("connect_timeout default should be 0, got %v", r.ConnectTimeout)
	}
}

func TestNextHopObjectFormResolvesTLS(t *testing.T) {
	// Given: the plain object form (uri only)
	plainForm := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence: []
`
	// When: parsed
	cfg, err := config.Parse([]byte(plainForm), "test.yaml")
	// Then: plain URI, udp default, no TLS
	if err != nil {
		t.Fatalf("plain form: unexpected error: %v", err)
	}
	if cfg.NextHop.URI != "sip:proxy.example.com" {
		t.Errorf("plain form uri = %q", cfg.NextHop.URI)
	}
	if cfg.NextHop.Transport != config.TransportUDP {
		t.Errorf("plain form transport = %q, want udp", cfg.NextHop.Transport)
	}
	if cfg.NextHop.Resolved != nil {
		t.Error("plain form should not resolve a TLS profile")
	}

	// Given: the object form with TLS
	objectForm := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
  transport: tls
  tls_profile: outbound
rtp:
  port_range: "10000-20000"
tls_profiles:
  outbound:
    cert: /etc/certs/out.pem
    key: /etc/certs/out.key
sequence: []
`
	// When: parsed
	cfg, err = config.Parse([]byte(objectForm), "test.yaml")
	// Then: TLS next hop resolved
	if err != nil {
		t.Fatalf("object form: unexpected error: %v", err)
	}
	if cfg.NextHop.Transport != config.TransportTLS {
		t.Errorf("object form transport = %q, want tls", cfg.NextHop.Transport)
	}
	if cfg.NextHop.Resolved == nil {
		t.Error("object form should resolve a TLS profile")
	}
}

func TestNonTLSEndpointNeedsNoProfile(t *testing.T) {
	// Given: a plain tcp app with no tls_profile
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence:
  - name: app
    uri: "sip:app.example.com"
    transport: tcp
`
	// When/Then: parse succeeds, no profile required
	cfg, err := config.Parse([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Sequence[0].Transport != config.TransportTCP {
		t.Errorf("transport = %q, want tcp", cfg.Sequence[0].Transport)
	}
	if cfg.Sequence[0].Resolved != nil {
		t.Error("non-TLS endpoint should have no resolved profile")
	}
}

func TestTLSProfileOnPlainEndpointRejected(t *testing.T) {
	// Given: a plain (udp) app carrying a tls_profile (R4 violation)
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
tls_profiles:
  outbound:
    cert: /etc/certs/out.pem
    key: /etc/certs/out.key
sequence:
  - name: app
    uri: "sip:app.example.com"
    tls_profile: outbound
`
	// When/Then: parse fails naming the endpoint
	_, err := config.Parse([]byte(yaml), "test.yaml")
	if err == nil {
		t.Fatal("expected error for tls_profile on plain endpoint")
	}
	if !strings.Contains(err.Error(), "tls_profile set but transport is") {
		t.Errorf("error %q missing R4 reason", err.Error())
	}
}

func TestInvalidMinVersionFails(t *testing.T) {
	// Given: a profile with an unsupported min_version
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
tls_profiles:
  outbound:
    cert: /etc/certs/out.pem
    key: /etc/certs/out.key
    min_version: tlsv1.1
sequence: []
`
	// When/Then: parse fails naming the profile and bad value
	_, err := config.Parse([]byte(yaml), "test.yaml")
	if err == nil {
		t.Fatal("expected error for invalid min_version")
	}
	if !strings.Contains(err.Error(), "unsupported min_version") || !strings.Contains(err.Error(), "tlsv1.1") {
		t.Errorf("error %q missing profile/value", err.Error())
	}
}

func TestInvalidConnectTimeoutFails(t *testing.T) {
	// Given: a profile with an unparseable connect_timeout
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
tls_profiles:
  outbound:
    cert: /etc/certs/out.pem
    key: /etc/certs/out.key
    connect_timeout: "soon"
sequence: []
`
	// When/Then: parse fails
	_, err := config.Parse([]byte(yaml), "test.yaml")
	if err == nil {
		t.Fatal("expected error for invalid connect_timeout")
	}
	if !strings.Contains(err.Error(), "invalid connect_timeout") {
		t.Errorf("error %q missing expected reason", err.Error())
	}
}

func TestEmptyVerifySubjectEntryFails(t *testing.T) {
	// Given: a profile with an empty verify_subjects entry
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
tls_profiles:
  outbound:
    cert: /etc/certs/out.pem
    key: /etc/certs/out.key
    verify_subjects:
      - "sip.example.com"
      - ""
sequence: []
`
	// When/Then: parse fails
	_, err := config.Parse([]byte(yaml), "test.yaml")
	if err == nil {
		t.Fatal("expected error for empty verify_subjects entry")
	}
	if !strings.Contains(err.Error(), "verify_subjects") {
		t.Errorf("error %q missing expected reason", err.Error())
	}
}

func TestNextHopMissingURIFails(t *testing.T) {
	// Given: an object next_hop present but with an empty uri
	yaml := `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  transport: udp
rtp:
  port_range: "10000-20000"
sequence: []
`
	// When/Then: parse fails naming the missing uri key
	_, err := config.Parse([]byte(yaml), "test.yaml")
	if err == nil {
		t.Fatal("expected error for next_hop with empty uri")
	}
	if !strings.Contains(err.Error(), "next_hop") || !strings.Contains(err.Error(), "uri") {
		t.Errorf("error %q should name next_hop and uri", err.Error())
	}
}

// Compile-time check: Load exists and has the right signature.
var _ = config.Load

// Ensure environment isolation: no os.Getenv calls needed in tests — t.Setenv is sufficient.
var _ = os.Setenv
