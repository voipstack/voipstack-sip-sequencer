package recorder

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

type audioStream struct {
	formats string
	rtpmaps []string
	fmtps   []string
}

// parseOffer parses a SDP body and returns one audioStream per m=audio section.
func parseOffer(sdp []byte) ([]audioStream, error) {
	var streams []audioStream
	var cur *audioStream

	for _, rawLine := range bytes.Split(sdp, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if strings.HasPrefix(line, "m=") {
			if strings.HasPrefix(line, "m=audio ") {
				fields := strings.Fields(line)
				if len(fields) < 4 {
					return nil, fmt.Errorf("parseOffer: malformed m=audio: %q", line)
				}
				streams = append(streams, audioStream{
					formats: strings.Join(fields[3:], " "),
				})
				cur = &streams[len(streams)-1]
			} else {
				cur = nil
			}
			continue
		}
		if cur == nil {
			continue
		}
		if strings.HasPrefix(line, "a=rtpmap:") {
			cur.rtpmaps = append(cur.rtpmaps, line)
		} else if strings.HasPrefix(line, "a=fmtp:") {
			cur.fmtps = append(cur.fmtps, line)
		}
	}

	if len(streams) == 0 {
		return nil, fmt.Errorf("parseOffer: no m=audio found")
	}
	return streams, nil
}

// buildAnswer builds a SDP answer matching the offer streams, placing our ports.
// Each stream is declared recvonly (we receive, never send).
func buildAnswer(offer []byte, host string, ports []int) ([]byte, error) {
	streams, err := parseOffer(offer)
	if err != nil {
		return nil, err
	}
	if len(ports) < len(streams) {
		return nil, fmt.Errorf("buildAnswer: need %d ports, have %d", len(streams), len(ports))
	}

	var b strings.Builder
	b.WriteString("v=0\r\n")
	b.WriteString("o=- 0 0 IN IP4 " + host + "\r\n")
	b.WriteString("s=-\r\n")
	b.WriteString("t=0 0\r\n")
	for i, s := range streams {
		b.WriteString("m=audio " + strconv.Itoa(ports[i]) + " RTP/AVP " + s.formats + "\r\n")
		b.WriteString("c=IN IP4 " + host + "\r\n")
		for _, r := range s.rtpmaps {
			b.WriteString(r + "\r\n")
		}
		for _, f := range s.fmtps {
			b.WriteString(f + "\r\n")
		}
		b.WriteString("a=recvonly\r\n")
	}
	return []byte(b.String()), nil
}
