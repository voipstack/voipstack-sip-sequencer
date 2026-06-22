package b2bua

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// SDP line prefixes the sequencer matches and emits. The sequencer is IPv4/RTP-audio
// only (PRD §5), so these are fixed.
const (
	sdpAudioMedia = "m=audio "  // RTP/AVP audio media (m=) line
	sdpConnIP4    = "c=IN IP4 " // IPv4 connection (c=) line
)

// extractAudioCodecs returns the format list from the first m=audio line plus
// all associated a=rtpmap: and a=fmtp: attributes that follow it.
func extractAudioCodecs(callOffer []byte) (formats string, rtpmaps, fmtps []string, err error) {
	var inAudio bool
	for _, rawLine := range bytes.Split(callOffer, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if strings.HasPrefix(line, "m=") {
			if inAudio {
				break
			}
			if strings.HasPrefix(line, sdpAudioMedia) {
				inAudio = true
				fields := strings.Fields(line)
				if len(fields) < 4 {
					return "", nil, nil, fmt.Errorf("extractAudioCodecs: malformed m=audio line: %q", line)
				}
				formats = strings.Join(fields[3:], " ")
			}
			continue
		}
		if !inAudio {
			continue
		}
		if strings.HasPrefix(line, "a=rtpmap:") {
			rtpmaps = append(rtpmaps, line)
		} else if strings.HasPrefix(line, "a=fmtp:") {
			fmtps = append(fmtps, line)
		}
	}
	if formats == "" {
		return "", nil, nil, fmt.Errorf("extractAudioCodecs: no audio m= line found")
	}
	return formats, rtpmaps, fmtps, nil
}

// AudioCodec is the agreed audio codec on one negotiated leg: the first payload type
// on the first m=audio line, resolved to the encoding name and clock rate it maps to.
type AudioCodec struct {
	PayloadType  int
	EncodingName string
	ClockRate    int
}

// Label is a human/metric-friendly name for the codec: its encoding name when known,
// or "pt<N>" for a payload type with neither an a=rtpmap nor a well-known static slot.
func (c AudioCodec) Label() string {
	if c.EncodingName != "" {
		return c.EncodingName
	}
	return "pt" + strconv.Itoa(c.PayloadType)
}

// staticAudioRTPMap is the subset of the RFC 3551 static RTP/AVP audio payload types we
// resolve when a negotiated SDP omits the a=rtpmap (legal for static types). It exists so
// a PBX that answers, say, G722 (pt 9) with no rtpmap still yields a comparable codec.
var staticAudioRTPMap = map[int]AudioCodec{
	0:  {PayloadType: 0, EncodingName: "PCMU", ClockRate: 8000},
	3:  {PayloadType: 3, EncodingName: "GSM", ClockRate: 8000},
	8:  {PayloadType: 8, EncodingName: "PCMA", ClockRate: 8000},
	9:  {PayloadType: 9, EncodingName: "G722", ClockRate: 8000},
	18: {PayloadType: 18, EncodingName: "G729", ClockRate: 8000},
}

// selectedAudioCodec extracts the agreed audio codec from a negotiated SDP: the first
// payload type listed on the first m=audio line, resolved to its encoding name and clock
// rate. A dynamic payload type takes its name/rate from the matching a=rtpmap; a payload
// type with no a=rtpmap falls back to the well-known static table (RFC 3551). An unknown
// static type with no rtpmap yields an empty name and zero clock rate (Label names it
// "pt<N>"). Pure, no I/O, CRLF-tolerant.
func selectedAudioCodec(sdp []byte) (AudioCodec, error) {
	var pt = -1
	var inAudio bool
	for _, rawLine := range bytes.Split(sdp, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if strings.HasPrefix(line, "m=") {
			if inAudio {
				break // past the first audio block
			}
			if !strings.HasPrefix(line, sdpAudioMedia) {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 4 {
				return AudioCodec{}, fmt.Errorf("selectedAudioCodec: malformed m=audio line: %q", line)
			}
			n, err := strconv.Atoi(fields[3])
			if err != nil {
				return AudioCodec{}, fmt.Errorf("selectedAudioCodec: bad payload type %q: %w", fields[3], err)
			}
			pt = n
			inAudio = true
			continue
		}
		if !inAudio {
			continue
		}
		if strings.HasPrefix(line, "a=rtpmap:") {
			if c, ok := parseRTPMap(line); ok && c.PayloadType == pt {
				return c, nil
			}
		}
	}
	if pt < 0 {
		return AudioCodec{}, fmt.Errorf("selectedAudioCodec: no audio m= line found")
	}
	if c, ok := staticAudioRTPMap[pt]; ok {
		return c, nil
	}
	return AudioCodec{PayloadType: pt}, nil
}

// parseRTPMap parses one "a=rtpmap:<pt> <encoding>/<clock>[/<channels>]" line into an
// AudioCodec. ok is false when the line is malformed.
func parseRTPMap(line string) (AudioCodec, bool) {
	body := strings.TrimPrefix(line, "a=rtpmap:")
	fields := strings.Fields(body)
	if len(fields) < 2 {
		return AudioCodec{}, false
	}
	pt, err := strconv.Atoi(fields[0])
	if err != nil {
		return AudioCodec{}, false
	}
	parts := strings.Split(fields[1], "/")
	if len(parts) < 2 {
		return AudioCodec{}, false
	}
	clock, err := strconv.Atoi(parts[1])
	if err != nil {
		return AudioCodec{}, false
	}
	return AudioCodec{PayloadType: pt, EncodingName: parts[0], ClockRate: clock}, true
}

// codecsMatch reports whether two negotiated legs agreed on the same audio format,
// comparing encoding name case-insensitively and clock rate. Payload type numbers may
// legitimately differ between legs (each side numbers dynamic types independently) and
// are not compared. Two unknown payload types (empty name) fall back to comparing the
// payload type number so they are not falsely flagged as a mismatch.
func codecsMatch(a, b AudioCodec) bool {
	if a.EncodingName == "" && b.EncodingName == "" {
		return a.PayloadType == b.PayloadType
	}
	return strings.EqualFold(a.EncodingName, b.EncodingName) && a.ClockRate == b.ClockRate
}

// offerIsWebRTC reports whether an SDP offer is a WebRTC (DTLS-SRTP) offer rather
// than a plain RTP/AVP offer. A browser offer uses a secure profile (RTP/SAVPF or
// RTP/SAVP) on its m=audio line and carries a DTLS a=fingerprint; either signal is
// sufficient. Plain RTP/AVP offers (no fingerprint) return false, so the plain
// anchoring path is untouched. Pure; CRLF-tolerant.
func offerIsWebRTC(sdp []byte) bool {
	for _, rawLine := range bytes.Split(sdp, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if strings.HasPrefix(line, sdpAudioMedia) {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				proto := fields[2]
				if strings.Contains(proto, "SAVPF") || strings.Contains(proto, "SAVP") {
					return true
				}
			}
		}
		if strings.HasPrefix(line, "a=fingerprint:") {
			return true
		}
	}
	return false
}

// trickleICEContentType is the MIME type carrying a trickled ICE candidate fragment
// in an in-dialog INFO (RFC 8840).
const trickleICEContentType = "application/trickle-ice-sdpfrag"

// isTrickleContentType reports whether a Content-Type value names the trickle-ICE
// SDP-fragment type. Case-insensitive; any ;-params and surrounding spaces are ignored.
func isTrickleContentType(ct string) bool {
	value := ct
	if i := strings.IndexByte(value, ';'); i >= 0 {
		value = value[:i]
	}
	return strings.EqualFold(strings.TrimSpace(value), trickleICEContentType)
}

// TrickleFragment is the parsed content of a trickle-ICE INFO body: the candidate
// attribute values it carries and whether it signals end-of-candidates.
type TrickleFragment struct {
	Candidates      []string
	EndOfCandidates bool
}

// parseTrickleFragment extracts every a=candidate: value (with the a= prefix stripped,
// i.e. "candidate:<foundation> …") and detects a=end-of-candidates. Pure, no I/O,
// CRLF-tolerant; an empty or garbage body yields the zero-value fragment.
func parseTrickleFragment(body []byte) TrickleFragment {
	var frag TrickleFragment
	for _, rawLine := range bytes.Split(body, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if strings.HasPrefix(line, "a=candidate:") {
			frag.Candidates = append(frag.Candidates, strings.TrimPrefix(line, "a="))
		} else if line == "a=end-of-candidates" {
			frag.EndOfCandidates = true
		}
	}
	return frag
}

// buildTapOffer builds a minimal SDP offer with two recvonly m=audio blocks.
// Stream 1 (rtpPort1) = caller direction; stream 2 (rtpPort2) = callee direction.
// Codec list is copied verbatim from callOffer.
func buildTapOffer(callOffer []byte, host string, rtpPort1, rtpPort2 int) ([]byte, error) {
	formats, rtpmaps, fmtps, err := extractAudioCodecs(callOffer)
	if err != nil {
		return nil, fmt.Errorf("buildTapOffer: %w", err)
	}

	var b strings.Builder
	b.WriteString("v=0\r\n")
	b.WriteString("o=- 0 0 IN IP4 " + host + "\r\n")
	b.WriteString("s=-\r\n")
	b.WriteString("t=0 0\r\n")

	for _, port := range []int{rtpPort1, rtpPort2} {
		b.WriteString(sdpAudioMedia + strconv.Itoa(port) + " RTP/AVP " + formats + "\r\n")
		b.WriteString(sdpConnIP4 + host + "\r\n")
		for _, r := range rtpmaps {
			b.WriteString(r + "\r\n")
		}
		for _, f := range fmtps {
			b.WriteString(f + "\r\n")
		}
		b.WriteString("a=recvonly\r\n")
	}

	return []byte(b.String()), nil
}

// buildInactiveOffer builds a minimal SDP offer with a single a=inactive m=audio block.
// Codec list is copied verbatim from callOffer so the app can negotiate (and decline) cleanly.
func buildInactiveOffer(callOffer []byte, host string) ([]byte, error) {
	formats, rtpmaps, fmtps, err := extractAudioCodecs(callOffer)
	if err != nil {
		return nil, fmt.Errorf("buildInactiveOffer: %w", err)
	}

	var b strings.Builder
	b.WriteString("v=0\r\n")
	b.WriteString("o=- 0 0 IN IP4 " + host + "\r\n")
	b.WriteString("s=-\r\n")
	b.WriteString("t=0 0\r\n")
	b.WriteString("m=audio 0 RTP/AVP " + formats + "\r\n")
	b.WriteString(sdpConnIP4 + host + "\r\n")
	for _, r := range rtpmaps {
		b.WriteString(r + "\r\n")
	}
	for _, f := range fmtps {
		b.WriteString(f + "\r\n")
	}
	b.WriteString("a=inactive\r\n")

	return []byte(b.String()), nil
}

// buildPlainOfferFromWebRTC derives a plain RTP/AVP offer from a WebRTC (DTLS-SRTP)
// offer: it carries the same negotiated audio codecs but strips every WebRTC-specific
// attribute (the SAVPF profile, ICE, DTLS fingerprint, rtcp-mux). The opposite leg is
// plain RTP and the codecs pass through end to end unchanged — the anchor never
// transcodes. The codec list (and so the payload types) is copied verbatim so raw
// payload forwarding stays correct. Pure; CRLF output; mirrors buildTapOffer.
func buildPlainOfferFromWebRTC(webrtcOffer []byte, host string, rtpPort int) ([]byte, error) {
	formats, rtpmaps, fmtps, err := extractAudioCodecs(webrtcOffer)
	if err != nil {
		return nil, fmt.Errorf("buildPlainOfferFromWebRTC: %w", err)
	}

	var b strings.Builder
	b.WriteString("v=0\r\n")
	b.WriteString("o=- 0 0 IN IP4 " + host + "\r\n")
	b.WriteString("s=-\r\n")
	b.WriteString("t=0 0\r\n")
	b.WriteString(sdpAudioMedia + strconv.Itoa(rtpPort) + " RTP/AVP " + formats + "\r\n")
	b.WriteString(sdpConnIP4 + host + "\r\n")
	for _, r := range rtpmaps {
		b.WriteString(r + "\r\n")
	}
	for _, f := range fmtps {
		b.WriteString(f + "\r\n")
	}
	b.WriteString("a=sendrecv\r\n")

	return []byte(b.String()), nil
}

// parseTapAnswer extracts the remote host and port for each of the two m=audio streams
// from an app's answer SDP. A missing stream or port 0 returns ("", 0) for that stream.
func parseTapAnswer(answer []byte) (h1 string, p1 int, h2 string, p2 int, err error) {
	type stream struct {
		host string
		port int
	}
	var streams []stream
	var sessionHost string
	var cur *stream

	for _, rawLine := range bytes.Split(answer, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if strings.HasPrefix(line, sdpAudioMedia) {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return "", 0, "", 0, fmt.Errorf("parseTapAnswer: malformed m=audio: %q", line)
			}
			port, e := strconv.Atoi(fields[1])
			if e != nil {
				return "", 0, "", 0, fmt.Errorf("parseTapAnswer: bad port in %q: %w", line, e)
			}
			streams = append(streams, stream{host: sessionHost, port: port})
			cur = &streams[len(streams)-1]
		} else if strings.HasPrefix(line, sdpConnIP4) {
			addr := strings.TrimPrefix(line, sdpConnIP4)
			if cur != nil {
				cur.host = addr
			} else {
				sessionHost = addr
			}
		}
	}

	if len(streams) < 2 {
		return "", 0, "", 0, fmt.Errorf("parseTapAnswer: expected 2 m=audio streams, got %d", len(streams))
	}
	return streams[0].host, streams[0].port, streams[1].host, streams[1].port, nil
}

// parsePortRange parses "min-max" and validates the range.
// min must be even, 0 < min < max.
func parsePortRange(s string) (min, max int, err error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("port range %q: expected format min-max", s)
	}
	min, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("port range %q: invalid min %q: %w", s, parts[0], err)
	}
	max, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("port range %q: invalid max %q: %w", s, parts[1], err)
	}
	if min <= 0 {
		return 0, 0, fmt.Errorf("port range %q: min %d must be positive", s, min)
	}
	if min%2 != 0 {
		return 0, 0, fmt.Errorf("port range %q: min %d must be even", s, min)
	}
	if min >= max {
		return 0, 0, fmt.Errorf("port range %q: min %d must be less than max %d", s, min, max)
	}
	return min, max, nil
}

// parseMedia extracts the remote RTP host and port from a SDP body.
// Uses the media-level c= if present, otherwise the session-level c=.
// Returns an error if no audio m= line is found.
func parseMedia(sdp []byte) (host string, rtpPort int, err error) {
	var sessionHost string
	var inAudio bool

	for _, rawLine := range bytes.Split(sdp, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if strings.HasPrefix(line, "m=") {
			inAudio = strings.HasPrefix(line, sdpAudioMedia)
			if inAudio {
				fields := strings.Fields(line)
				if len(fields) < 2 {
					return "", 0, fmt.Errorf("parseMedia: malformed m= line: %q", line)
				}
				rtpPort, err = strconv.Atoi(fields[1])
				if err != nil {
					return "", 0, fmt.Errorf("parseMedia: bad port in m= line %q: %w", line, err)
				}
				// use session host unless a media-level c= follows
				host = sessionHost
			}
		} else if strings.HasPrefix(line, sdpConnIP4) {
			addr := strings.TrimPrefix(line, sdpConnIP4)
			if inAudio {
				host = addr
			} else {
				sessionHost = addr
				if host == "" {
					host = addr
				}
			}
		}
	}

	if rtpPort == 0 {
		return "", 0, fmt.Errorf("parseMedia: no audio m= line found")
	}
	if host == "" {
		return "", 0, fmt.Errorf("parseMedia: no c= address found")
	}
	// The c= address must be an IP literal: the sequencer anchors and relays to a
	// concrete IP:port and resolves nothing. An FQDN or garbage host yields a nil
	// net.ParseIP, which downstream would wrap in a non-nil *net.UDPAddr{IP: nil} that
	// slips past the relay's nil-address guard and silently kills that direction.
	if net.ParseIP(host) == nil {
		return "", 0, fmt.Errorf("parseMedia: c= address %q is not an IP literal", host)
	}
	return host, rtpPort, nil
}

// rewriteToAnchor rewrites c= and the first audio m= port to host/rtpPort.
// All other lines are passed through verbatim. CRLF line endings are preserved.
func rewriteToAnchor(sdp []byte, host string, rtpPort int) ([]byte, error) {
	if host == "" {
		return nil, fmt.Errorf("rewriteToAnchor: empty host")
	}
	if rtpPort <= 0 {
		return nil, fmt.Errorf("rewriteToAnchor: invalid port %d", rtpPort)
	}

	var out bytes.Buffer
	audioRewritten := false
	cRewritten := false

	lines := bytes.Split(sdp, []byte("\n"))
	for i, rawLine := range lines {
		hasCR := bytes.HasSuffix(rawLine, []byte("\r"))
		line := strings.TrimRight(string(rawLine), "\r")

		var newLine string
		switch {
		case strings.HasPrefix(line, sdpConnIP4) && !cRewritten:
			newLine = sdpConnIP4 + host
			cRewritten = true
		case strings.HasPrefix(line, sdpAudioMedia) && !audioRewritten:
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return nil, fmt.Errorf("rewriteToAnchor: malformed m= line: %q", line)
			}
			fields[1] = strconv.Itoa(rtpPort)
			newLine = strings.Join(fields, " ")
			audioRewritten = true
		default:
			newLine = line
		}

		out.WriteString(newLine)
		if hasCR {
			out.WriteByte('\r')
		}
		if i < len(lines)-1 {
			out.WriteByte('\n')
		}
	}

	return out.Bytes(), nil
}
