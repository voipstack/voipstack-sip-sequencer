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
next_hop: "sip:proxy.example.com"
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
	if cfg.NextHop != "sip:proxy.example.com" {
		t.Errorf("next_hop = %q, want %q", cfg.NextHop, "sip:proxy.example.com")
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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
	if cfg.NextHop == "sip:env-injected.example.com" {
		t.Error("next_hop was read from environment variable — must come only from YAML")
	}
	if cfg.NextHop != "sip:proxy.example.com" {
		t.Errorf("next_hop = %q, want %q", cfg.NextHop, "sip:proxy.example.com")
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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
next_hop: "sip:proxy.example.com"
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

// Compile-time check: Load exists and has the right signature.
var _ = config.Load

// Ensure environment isolation: no os.Getenv calls needed in tests — t.Setenv is sufficient.
var _ = os.Setenv
