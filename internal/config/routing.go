package config

import (
	"fmt"
	"regexp"
)

// RoutingRule filters which inbound requests an application receives. Every
// specified field must match for the rule to match; an empty rule (no fields set)
// matches every request, preserving the default "always route" behavior.
//
// From and To are matched as regular expressions against the SIP URI in its
// string form (e.g. "sip:alice@example.com"). Method is matched verbatim,
// case-insensitively against the SIP method token (e.g. "INVITE").
//
// The compiled patterns live on the resolved Application (FromRe, ToRe) so the
// YAML-facing struct stays a plain data value; Match is pure and side-effect free.
type RoutingRule struct {
	From   string `yaml:"from"`
	To     string `yaml:"to"`
	Method string `yaml:"method"`
}

// RoutingInput is the request shape the matcher needs. Keeping it a plain struct
// of strings lets the pure matcher live in the config package with no SIP
// library dependency; the bridge converts a sipgo request into this input at the
// edge.
type RoutingInput struct {
	Method string
	From   string
	To     string
}

// ResolvedRouting is a RoutingRule with its regex patterns compiled. The zero
// value (nil patterns) matches every request.
type ResolvedRouting struct {
	FromRe *regexp.Regexp
	ToRe   *regexp.Regexp
	Method string
}

// Matches reports whether req satisfies every specified field of the rule. A nil
// resolved rule (no routing block) matches everything. Each present field is an
// independent AND condition; absent fields are wildcards.
func (r *ResolvedRouting) Matches(req RoutingInput) bool {
	if r == nil {
		return true
	}
	if r.Method != "" && !equalMethod(r.Method, req.Method) {
		return false
	}
	if r.FromRe != nil && !r.FromRe.MatchString(req.From) {
		return false
	}
	if r.ToRe != nil && !r.ToRe.MatchString(req.To) {
		return false
	}
	return true
}

// resolveRoutingRule compiles the rule's regex patterns and normalizes the method. A
// nil rule yields a nil resolved rule (matches everything). An invalid regex is
// returned wrapped with field context.
func resolveRoutingRule(r *RoutingRule) (*ResolvedRouting, error) {
	if r == nil {
		return nil, nil
	}
	res := &ResolvedRouting{Method: r.Method}
	if r.From != "" {
		re, err := regexp.Compile(r.From)
		if err != nil {
			return nil, fmt.Errorf("routing.from %q: %w", r.From, err)
		}
		res.FromRe = re
	}
	if r.To != "" {
		re, err := regexp.Compile(r.To)
		if err != nil {
			return nil, fmt.Errorf("routing.to %q: %w", r.To, err)
		}
		res.ToRe = re
	}
	return res, nil
}

// equalMethod compares two SIP method tokens case-insensitively. SIP methods are
// defined case-sensitive (RFC 3261 §7.1) but a tolerant compare avoids operator
// typos in YAML (e.g. "invite" vs "INVITE").
func equalMethod(a, b string) bool {
	la, lb := len(a), len(b)
	if la != lb {
		return false
	}
	for i := 0; i < la; i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
