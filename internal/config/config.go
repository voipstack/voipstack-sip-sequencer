// Package config provides loading and validation of the sequencer YAML configuration.
package config

import (
	"bytes"
	"fmt"
	"net"
	"os"

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

// SIP holds SIP listener configuration.
type SIP struct {
	Listen string `yaml:"listen"`
}

// RTP holds RTP port range configuration.
type RTP struct {
	PortRange string `yaml:"port_range"`
}

// Observability holds the optional metrics/health HTTP listener configuration.
// An empty Listen disables observability (no HTTP server).
type Observability struct {
	Listen string `yaml:"listen"`
}

// Application describes a single external SIP application in the sequence chain.
type Application struct {
	Name      string        `yaml:"name"`
	URI       string        `yaml:"uri"`
	OnFailure FailurePolicy `yaml:"on_failure"`
	Media     MediaMode     `yaml:"media"`
}

// Config is the validated, in-memory representation of the operator-supplied YAML file.
type Config struct {
	SIP           SIP           `yaml:"sip"`
	NextHop       string        `yaml:"next_hop"`
	RTP           RTP           `yaml:"rtp"`
	Sequence      []Application `yaml:"sequence"`
	LogLevel      LogLevel      `yaml:"log_level"`
	Observability Observability `yaml:"observability"`
}

// rawConfig mirrors Config but tracks presence of the sequence key via a pointer.
type rawConfig struct {
	SIP           SIP            `yaml:"sip"`
	NextHop       string         `yaml:"next_hop"`
	RTP           RTP            `yaml:"rtp"`
	Sequence      *[]Application `yaml:"sequence"`
	LogLevel      LogLevel       `yaml:"log_level"`
	Observability Observability  `yaml:"observability"`
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
		NextHop:       raw.NextHop,
		RTP:           raw.RTP,
		LogLevel:      raw.LogLevel,
		Observability: raw.Observability,
	}
	sequencePresent := raw.Sequence != nil
	if sequencePresent {
		cfg.Sequence = *raw.Sequence
	}

	applyDefaults(&cfg)

	if err := validate(cfg, sequencePresent); err != nil {
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
	}
}

func validate(c Config, sequencePresent bool) error {
	if c.SIP.Listen == "" {
		return fmt.Errorf("missing required key %q", "sip.listen")
	}
	if c.NextHop == "" {
		return fmt.Errorf("missing required key %q", "next_hop")
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
	return nil
}
