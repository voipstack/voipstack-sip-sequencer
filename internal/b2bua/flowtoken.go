package b2bua

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

// Flow is the webphone's signaling flow: the source address its long-lived
// WebSocket connection arrived on, plus that connection's transport. It is a value
// type and is never stored server-side — it is encoded (HMAC-signed) into a Path
// token and recovered from an inbound Route, so the registrar's binding stays the
// only durable record.
type Flow struct {
	Addr      string
	Transport string
}

// mintFlowToken encodes f into an opaque, URL-safe token and signs it with secret.
// The token is "<payload>.<mac>", both base64url without padding, so it is safe to
// carry as a SIP URI user-part. Pure: its result depends only on its arguments.
func mintFlowToken(f Flow, secret []byte) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(f.Transport + "|" + f.Addr))
	mac := base64.RawURLEncoding.EncodeToString(flowMAC(secret, payload))
	return payload + "." + mac
}

// parseFlowToken verifies token's MAC against secret with a constant-time compare and,
// only on a match, decodes the Flow. A tampered, malformed, or foreign-signed token
// returns an error and never a Flow — so a caller can forward only to an address it
// recovered here.
func parseFlowToken(token string, secret []byte) (Flow, error) {
	payload, macStr, ok := strings.Cut(token, ".")
	if !ok {
		return Flow{}, fmt.Errorf("invalid flow token: missing mac")
	}
	mac, err := base64.RawURLEncoding.DecodeString(macStr)
	if err != nil {
		return Flow{}, fmt.Errorf("invalid flow token: decode mac: %w", err)
	}
	if !hmac.Equal(mac, flowMAC(secret, payload)) {
		return Flow{}, fmt.Errorf("invalid flow token: mac mismatch")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return Flow{}, fmt.Errorf("invalid flow token: decode payload: %w", err)
	}
	transport, addr, ok := strings.Cut(string(raw), "|")
	if !ok {
		return Flow{}, fmt.Errorf("invalid flow token: malformed payload")
	}
	return Flow{Addr: addr, Transport: transport}, nil
}

// flowMAC computes the HMAC-SHA256 of the token payload string under secret.
func flowMAC(secret []byte, payload string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(payload))
	return m.Sum(nil)
}
