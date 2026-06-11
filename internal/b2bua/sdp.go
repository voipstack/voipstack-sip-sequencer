package b2bua

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
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
			if strings.HasPrefix(line, "m=audio ") {
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

// offerIsWebRTC reports whether an SDP offer is a WebRTC (DTLS-SRTP) offer rather
// than a plain RTP/AVP offer. A browser offer uses a secure profile (RTP/SAVPF or
// RTP/SAVP) on its m=audio line and carries a DTLS a=fingerprint; either signal is
// sufficient. Plain RTP/AVP offers (no fingerprint) return false, so the plain
// anchoring path is untouched. Pure; CRLF-tolerant.
func offerIsWebRTC(sdp []byte) bool {
	for _, rawLine := range bytes.Split(sdp, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if strings.HasPrefix(line, "m=audio ") {
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
		b.WriteString("m=audio " + strconv.Itoa(port) + " RTP/AVP " + formats + "\r\n")
		b.WriteString("c=IN IP4 " + host + "\r\n")
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
	b.WriteString("c=IN IP4 " + host + "\r\n")
	for _, r := range rtpmaps {
		b.WriteString(r + "\r\n")
	}
	for _, f := range fmtps {
		b.WriteString(f + "\r\n")
	}
	b.WriteString("a=inactive\r\n")

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
		if strings.HasPrefix(line, "m=audio ") {
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
		} else if strings.HasPrefix(line, "c=IN IP4 ") {
			addr := strings.TrimPrefix(line, "c=IN IP4 ")
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
			inAudio = strings.HasPrefix(line, "m=audio ")
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
		} else if strings.HasPrefix(line, "c=IN IP4 ") {
			addr := strings.TrimPrefix(line, "c=IN IP4 ")
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
		case strings.HasPrefix(line, "c=IN IP4 ") && !cRewritten:
			newLine = "c=IN IP4 " + host
			cRewritten = true
		case strings.HasPrefix(line, "m=audio ") && !audioRewritten:
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
