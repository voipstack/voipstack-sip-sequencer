package b2bua

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// Flow is the webphone's signaling flow: the source address its long-lived connection
// arrived on, plus that connection's transport. It is a value type and is never stored
// server-side — it is packed into a compact, opaque Path token (RFC 5626 §5.1, the
// stateless flow-token variant) and recovered from an inbound Route, so the registrar's
// binding stays the only durable record.
//
// SECURITY: the token is deliberately NOT signed — this sequencer runs in a trusted
// network where only trusted peers can present a Route on its signaling address. A
// token is therefore taken at face value: parseFlowToken decodes it and the request is
// forwarded to whatever address it yields. An unsigned token is a request-forwarding
// (SSRF) primitive, so this listener MUST NOT be exposed to untrusted clients. If that
// assumption ever changes, restore the HMAC (see git history) so a forged Route cannot
// steer the sequencer to an arbitrary address.
type Flow struct {
	Addr      string
	Transport string
}

// transport codes for the token's 1-byte transport field. The set is closed (sipgo's
// transports); an unknown value is rejected rather than guessed.
var (
	transportToCode = map[string]byte{"udp": 0, "tcp": 1, "tls": 2, "ws": 3, "wss": 4}
	codeToTransport = []string{"udp", "tcp", "tls", "ws", "wss"}
)

// mintFlowToken packs f into a compact, URL-safe token: a binary blob
// [transport:1][ip:4|16][port:2], base64url-encoded (no padding, so it is safe in a SIP
// URI user-part). IPv4 yields a 7-byte blob → 10 chars; IPv6 a 19-byte blob → 26 chars.
// Pure: its result depends only on its argument. It errors on a flow it cannot encode
// (unknown transport, non-IP host) rather than emitting a token that will not parse.
func mintFlowToken(f Flow) (string, error) {
	code, ok := transportToCode[strings.ToLower(f.Transport)]
	if !ok {
		return "", fmt.Errorf("flow token: unknown transport %q", f.Transport)
	}
	host, portStr, err := net.SplitHostPort(f.Addr)
	if err != nil {
		return "", fmt.Errorf("flow token: split addr %q: %w", f.Addr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("flow token: addr host %q is not an IP", host)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return "", fmt.Errorf("flow token: port %q: %w", portStr, err)
	}

	blob := []byte{code}
	if v4 := ip.To4(); v4 != nil {
		blob = append(blob, v4...)
	} else {
		blob = append(blob, ip.To16()...)
	}
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(port))
	blob = append(blob, p[:]...)

	return base64.RawURLEncoding.EncodeToString(blob), nil
}

// parseFlowToken decodes a token minted by mintFlowToken back into a Flow. A malformed
// token (bad base64, wrong length, or an unknown transport code) returns an error and
// no Flow, so a caller forwards only to an address it actually recovered. NOTE: there
// is no signature to verify — any well-formed token is accepted (see Flow's security
// note); this only guards against garbage, not against a forged-but-valid token.
func parseFlowToken(token string) (Flow, error) {
	blob, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Flow{}, fmt.Errorf("flow token: decode: %w", err)
	}
	// 1 transport byte + (4 or 16) IP bytes + 2 port bytes.
	var ipLen int
	switch len(blob) {
	case 1 + net.IPv4len + 2:
		ipLen = net.IPv4len
	case 1 + net.IPv6len + 2:
		ipLen = net.IPv6len
	default:
		return Flow{}, fmt.Errorf("flow token: bad length %d", len(blob))
	}
	if int(blob[0]) >= len(codeToTransport) {
		return Flow{}, fmt.Errorf("flow token: unknown transport code %d", blob[0])
	}

	transport := codeToTransport[blob[0]]
	ip := net.IP(blob[1 : 1+ipLen])
	port := binary.BigEndian.Uint16(blob[1+ipLen:])
	return Flow{
		Transport: transport,
		Addr:      net.JoinHostPort(ip.String(), strconv.Itoa(int(port))),
	}, nil
}
