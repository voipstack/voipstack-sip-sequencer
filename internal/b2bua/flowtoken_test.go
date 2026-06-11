package b2bua

import (
	"strings"
	"testing"
)

// Given a Flow and a secret; When minted then parsed with the same secret;
// Then the original Flow is recovered.
func TestFlowTokenRoundTrips(t *testing.T) {
	secret := []byte("test-secret-0123456789abcdef")
	in := Flow{Addr: "203.0.113.7:51234", Transport: "ws"}

	token := mintFlowToken(in, secret)
	out, err := parseFlowToken(token, secret)
	if err != nil {
		t.Fatalf("parseFlowToken: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", out, in)
	}
}

// The token must be safe to carry as a SIP URI user-part: base64url payload and mac
// joined by a dot, no characters that would break URI parsing.
func TestFlowTokenIsURISafe(t *testing.T) {
	secret := []byte("secret")
	token := mintFlowToken(Flow{Addr: "[2001:db8::1]:5060", Transport: "wss"}, secret)
	const allowed = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_."
	for _, r := range token {
		if !strings.ContainsRune(allowed, r) {
			t.Fatalf("token contains URI-unsafe rune %q in %q", r, token)
		}
	}
}

// Given a token; When its payload is tampered; Then parse fails (MAC no longer matches).
func TestFlowTokenTamperedPayloadFails(t *testing.T) {
	secret := []byte("secret")
	token := mintFlowToken(Flow{Addr: "10.0.0.1:5060", Transport: "ws"}, secret)

	payload, mac, _ := strings.Cut(token, ".")
	// Flip a byte in the payload while keeping it valid base64url.
	b := []byte(payload)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	tampered := string(b) + "." + mac

	if _, err := parseFlowToken(tampered, secret); err == nil {
		t.Fatal("expected error for tampered payload, got nil")
	}
}

// Given a token; When its MAC is tampered; Then parse fails.
func TestFlowTokenTamperedMACFails(t *testing.T) {
	secret := []byte("secret")
	token := mintFlowToken(Flow{Addr: "10.0.0.1:5060", Transport: "ws"}, secret)

	payload, mac, _ := strings.Cut(token, ".")
	b := []byte(mac)
	if b[len(b)-1] == 'A' {
		b[len(b)-1] = 'B'
	} else {
		b[len(b)-1] = 'A'
	}
	tampered := payload + "." + string(b)

	if _, err := parseFlowToken(tampered, secret); err == nil {
		t.Fatal("expected error for tampered mac, got nil")
	}
}

// Given a token minted with one secret; When parsed with a different secret;
// Then parse fails — a foreign-signed token never yields a Flow.
func TestFlowTokenForeignSecretFails(t *testing.T) {
	token := mintFlowToken(Flow{Addr: "10.0.0.1:5060", Transport: "ws"}, []byte("secret-A"))
	if _, err := parseFlowToken(token, []byte("secret-B")); err == nil {
		t.Fatal("expected error for foreign secret, got nil")
	}
}

// Malformed input must be rejected with an error, never a panic.
func TestFlowTokenMalformedRejected(t *testing.T) {
	secret := []byte("secret")
	for _, tok := range []string{"", "no-dot", ".", "a.b.c", "$$$.$$$", "validlooking."} {
		if _, err := parseFlowToken(tok, secret); err == nil {
			t.Fatalf("expected error for malformed token %q, got nil", tok)
		}
	}
}
