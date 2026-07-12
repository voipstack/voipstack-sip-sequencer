package config_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// ── pure match logic ─────────────────────────────────────────────────────────

// Given a nil rule (no routing block); When Matches is called for any request;
// Then it matches everything (backward compatible — every app receives the call).
func TestNilRuleMatchesEverything(t *testing.T) {
	var r *config.ResolvedRouting
	for _, in := range []config.RoutingInput{
		{Method: "INVITE", From: "sip:alice@caller.example.com", To: "sip:bob@pbx.example.com"},
		{Method: "REGISTER", From: "sip:x@x", To: "sip:y@y"},
		{},
	} {
		if !r.Matches(in) {
			t.Fatalf("nil rule must match %+v", in)
		}
	}
}

// Given a rule with only a method; When the request method differs; Then no match.
func TestMethodRuleMatchesOnlyThatMethod(t *testing.T) {
	r := &config.ResolvedRouting{Method: "INVITE"}
	if !r.Matches(config.RoutingInput{Method: "INVITE", From: "sip:a@h", To: "sip:b@h"}) {
		t.Fatal("INVITE rule must match an INVITE request")
	}
	if r.Matches(config.RoutingInput{Method: "REGISTER", From: "sip:a@h", To: "sip:b@h"}) {
		t.Fatal("INVITE rule must not match a REGISTER request")
	}
}

// Given a method rule in lowercase; When the request is uppercase INVITE;
// Then it matches — method comparison is case-insensitive.
func TestMethodRuleIsCaseInsensitive(t *testing.T) {
	r := &config.ResolvedRouting{Method: "invite"}
	if !r.Matches(config.RoutingInput{Method: "INVITE"}) {
		t.Fatal("lowercase rule must match uppercase method")
	}
}

// Given a from regex anchoring the user part; When the caller URI matches;
// Then the rule matches; When the user differs, it does not.
func TestFromRegexMatchesURI(t *testing.T) {
	r := &config.ResolvedRouting{FromRe: regexp.MustCompile(`^sip:alice@`)}
	if !r.Matches(config.RoutingInput{Method: "INVITE", From: "sip:alice@caller.example.com", To: "sip:bob@pbx"}) {
		t.Fatal("anchored from rule must match alice's URI")
	}
	if r.Matches(config.RoutingInput{Method: "INVITE", From: "sip:bob@caller.example.com", To: "sip:bob@pbx"}) {
		t.Fatal("from rule must not match a different user")
	}
}

// Given separate from and to regexes; When both match; Then the rule matches.
// When either fails, it does not — every field is an AND condition.
func TestFromAndToAreANDed(t *testing.T) {
	r := &config.ResolvedRouting{
		FromRe: regexp.MustCompile(`alice@`),
		ToRe:   regexp.MustCompile(`^sip:support@`),
	}
	if !r.Matches(config.RoutingInput{From: "sip:alice@x", To: "sip:support@pbx"}) {
		t.Fatal("both fields match → rule matches")
	}
	if r.Matches(config.RoutingInput{From: "sip:alice@x", To: "sip:sales@pbx"}) {
		t.Fatal("to fails → rule must not match")
	}
	if r.Matches(config.RoutingInput{From: "sip:carol@x", To: "sip:support@pbx"}) {
		t.Fatal("from fails → rule must not match")
	}
}

// Given a rule with method + from + to; When all three match; Then match.
// When any one fails, no match.
func TestAllFieldsANDed(t *testing.T) {
	r := &config.ResolvedRouting{
		Method: "INVITE",
		FromRe: regexp.MustCompile(`alice@`),
		ToRe:   regexp.MustCompile(`^sip:bob@`),
	}
	good := config.RoutingInput{Method: "INVITE", From: "sip:alice@caller", To: "sip:bob@pbx"}
	if !r.Matches(good) {
		t.Fatalf("all-match input %+v must match", good)
	}
	if r.Matches(config.RoutingInput{Method: "OPTIONS", From: good.From, To: good.To}) {
		t.Fatal("method mismatch must not match")
	}
}

// Given an empty (zero-value) resolved rule; When matching any request;
// Then it matches every request — no field set means no constraint.
func TestEmptyResolvedRuleMatchesAll(t *testing.T) {
	r := &config.ResolvedRouting{}
	if !r.Matches(config.RoutingInput{Method: "INVITE", From: "sip:a@b", To: "sip:c@d"}) {
		t.Fatal("zero-value rule must match everything")
	}
	if !r.Matches(config.RoutingInput{}) {
		t.Fatal("zero-value rule must match an empty input")
	}
}

// ── config parsing ──────────────────────────────────────────────────────────

const baseYAML = `
sip:
  listen: "0.0.0.0:5060"
next_hop:
  uri: "sip:proxy.example.com"
rtp:
  port_range: "10000-20000"
sequence:
`

func appWithRoutingYAML(routing string) string {
	return baseYAML + "  - name: app1\n    uri: \"sip:app1.example.com\"\n" + routing
}

// Given a sequence app with a routing.from regex; When Parse is called;
// Then the rule is compiled onto RoutingRe and matches an alice URI.
func TestParseCompilesRoutingFromRegex(t *testing.T) {
	yaml := appWithRoutingYAML("    routing:\n      from: \"^sip:alice@\"\n")
	cfg, err := config.Parse([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Sequence[0].RoutingRe == nil {
		t.Fatal("RoutingRe should be resolved when a routing block is present")
	}
	if !cfg.Sequence[0].RoutingRe.Matches(config.RoutingInput{From: "sip:alice@caller.example.com"}) {
		t.Fatal("compiled from regex must match alice's URI")
	}
	if cfg.Sequence[0].RoutingRe.Matches(config.RoutingInput{From: "sip:bob@caller.example.com"}) {
		t.Fatal("compiled from regex must not match bob's URI")
	}
}

// Given a sequence app with from + to + method in routing; When Parse is called;
// Then all three are resolved and AND-matched at runtime.
func TestParseCompilesRoutingAllFields(t *testing.T) {
	yaml := appWithRoutingYAML("    routing:\n      from: \"alice@\"\n      to: \"^sip:support@\"\n      method: \"INVITE\"\n")
	cfg, err := config.Parse([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := cfg.Sequence[0].RoutingRe
	if r == nil || r.FromRe == nil || r.ToRe == nil || r.Method != "INVITE" {
		t.Fatalf("routing not fully resolved: %+v", r)
	}
	if !r.Matches(config.RoutingInput{Method: "INVITE", From: "sip:alice@x", To: "sip:support@pbx"}) {
		t.Fatal("fully-matching input must match")
	}
	if r.Matches(config.RoutingInput{Method: "OPTIONS", From: "sip:alice@x", To: "sip:support@pbx"}) {
		t.Fatal("method mismatch must not match")
	}
}

// Given an app with no routing block; When Parse is called; Then RoutingRe is nil
// (matches everything) and backward compatibility is preserved.
func TestParseOmittedRoutingYieldsNilResolved(t *testing.T) {
	cfg, err := config.Parse([]byte(completeYAML), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, app := range cfg.Sequence {
		if app.RoutingRe != nil {
			t.Errorf("sequence[%d] %q: RoutingRe must be nil when no routing block", i, app.Name)
		}
		if !app.RoutingRe.Matches(config.RoutingInput{Method: "INVITE"}) {
			t.Errorf("sequence[%d] %q: nil rule must match any request", i, app.Name)
		}
	}
}

// Given an app with an invalid from regex; When Parse is called; Then the error
// names the field and the bad pattern, aborting config load.
func TestParseFailsOnInvalidFromRegex(t *testing.T) {
	yaml := appWithRoutingYAML("    routing:\n      from: \"(unclosed\"\n")
	_, err := config.Parse([]byte(yaml), "bad.yaml")
	if err == nil {
		t.Fatal("expected error for invalid from regex, got nil")
	}
	if !strings.Contains(err.Error(), "routing.from") || !strings.Contains(err.Error(), "(unclosed") {
		t.Errorf("error %q must name routing.from and the bad pattern", err.Error())
	}
}

// Given an app with an invalid to regex; When Parse is called; Then the error
// names routing.to and the bad pattern.
func TestParseFailsOnInvalidToRegex(t *testing.T) {
	yaml := appWithRoutingYAML("    routing:\n      to: \"[bad\"\n")
	_, err := config.Parse([]byte(yaml), "bad.yaml")
	if err == nil {
		t.Fatal("expected error for invalid to regex, got nil")
	}
	if !strings.Contains(err.Error(), "routing.to") || !strings.Contains(err.Error(), "[bad") {
		t.Errorf("error %q must name routing.to and the bad pattern", err.Error())
	}
}

// Given an app with an empty routing block (present but no fields); When Parse;
// Then RoutingRe is non-nil but matches everything (wildcard).
func TestParseEmptyRoutingBlockMatchesAll(t *testing.T) {
	yaml := appWithRoutingYAML("    routing: {}\n")
	cfg, err := config.Parse([]byte(yaml), "test.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r := cfg.Sequence[0].RoutingRe
	if r == nil {
		t.Fatal("present routing block must yield a non-nil resolved rule")
	}
	if !r.Matches(config.RoutingInput{Method: "INVITE", From: "sip:anyone@anywhere", To: "sip:anywhere@anywhere"}) {
		t.Fatal("empty routing block must match every request")
	}
}
