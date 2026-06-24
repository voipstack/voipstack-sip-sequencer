package b2bua

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Given a Flow; When packed into a token then parsed; Then the original Flow is
// recovered — across every transport and both IPv4 and IPv6.
func TestFlowTokenRoundTrips(t *testing.T) {
	cases := []Flow{
		{Addr: "203.0.113.7:51234", Transport: "ws"},
		{Addr: "192.168.56.1:5064", Transport: "udp"},
		{Addr: "10.0.0.1:5060", Transport: "tcp"},
		{Addr: "10.0.0.2:5061", Transport: "tls"},
		{Addr: "[2001:db8::1]:5060", Transport: "wss"},
	}
	for _, in := range cases {
		token, err := mintFlowToken(in)
		if err != nil {
			t.Fatalf("mintFlowToken(%+v): %v", in, err)
		}
		out, err := parseFlowToken(token)
		if err != nil {
			t.Fatalf("parseFlowToken(%q): %v", token, err)
		}
		if out != in {
			t.Fatalf("round-trip mismatch: got %+v, want %+v", out, in)
		}
	}
}

// The transport is canonicalized to lower case (sipgo reports it upper case), so a
// minted "UDP" flow parses back as "udp".
func TestFlowTokenNormalizesTransportCase(t *testing.T) {
	token, err := mintFlowToken(Flow{Addr: "10.0.0.1:5060", Transport: "UDP"})
	if err != nil {
		t.Fatalf("mintFlowToken: %v", err)
	}
	out, err := parseFlowToken(token)
	if err != nil {
		t.Fatalf("parseFlowToken: %v", err)
	}
	if out.Transport != "udp" {
		t.Fatalf("transport = %q, want udp", out.Transport)
	}
}

// The token must be safe to carry as a SIP URI user-part: a single base64url blob with
// no characters that would break URI parsing.
func TestFlowTokenIsURISafe(t *testing.T) {
	token, err := mintFlowToken(Flow{Addr: "[2001:db8::1]:5060", Transport: "wss"})
	if err != nil {
		t.Fatalf("mintFlowToken: %v", err)
	}
	const allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_"
	for _, r := range token {
		if !strings.ContainsRune(allowed, r) {
			t.Fatalf("token contains URI-unsafe rune %q in %q", r, token)
		}
	}
}

// The whole point of the binary packing: an IPv4 flow token is tiny (7-byte blob → 10
// base64url chars). Lock that in so a future change cannot silently re-inflate it.
func TestFlowTokenIsCompact(t *testing.T) {
	token, err := mintFlowToken(Flow{Addr: "192.168.56.1:5064", Transport: "udp"})
	if err != nil {
		t.Fatalf("mintFlowToken: %v", err)
	}
	if len(token) > 12 {
		t.Fatalf("ipv4 token = %d chars (%q), want <= 12", len(token), token)
	}
}

// Malformed input must be rejected with an error (never a Flow, never a panic): bad
// base64, a valid-base64 blob of the wrong length, and a well-sized blob whose
// transport byte is out of range.
func TestFlowTokenMalformedRejected(t *testing.T) {
	badCode := base64.RawURLEncoding.EncodeToString([]byte{9, 1, 2, 3, 4, 5, 6}) // 7 bytes, code 9
	for _, tok := range []string{"", "!!!", "AAAA", badCode} {
		if _, err := parseFlowToken(tok); err == nil {
			t.Fatalf("expected error for malformed token %q, got nil", tok)
		}
	}
}

// A flow the token cannot represent (unknown transport, or a host that is not an IP)
// must fail at mint rather than emit a token that will not parse.
func TestFlowTokenMintRejectsUnencodableFlow(t *testing.T) {
	if _, err := mintFlowToken(Flow{Addr: "10.0.0.1:5060", Transport: "sctp"}); err == nil {
		t.Fatal("expected error for unknown transport, got nil")
	}
	if _, err := mintFlowToken(Flow{Addr: "example.com:5060", Transport: "udp"}); err == nil {
		t.Fatal("expected error for non-IP host, got nil")
	}
}
