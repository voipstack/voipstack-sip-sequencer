package recorder

import (
	"bytes"
	"strings"
	"testing"
)

const tapOffer = "v=0\r\n" +
	"o=- 0 0 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"m=audio 20000 RTP/AVP 0 8\r\n" +
	"c=IN IP4 127.0.0.1\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=rtpmap:8 PCMA/8000\r\n" +
	"a=fmtp:0 0\r\n" +
	"a=recvonly\r\n" +
	"m=audio 20002 RTP/AVP 0 8\r\n" +
	"c=IN IP4 127.0.0.1\r\n" +
	"a=rtpmap:0 PCMU/8000\r\n" +
	"a=rtpmap:8 PCMA/8000\r\n" +
	"a=fmtp:0 0\r\n" +
	"a=recvonly\r\n"

// Given a two-stream recvonly tap offer; When buildAnswer is called with two ports;
// Then the answer has two m=audio with our ports, a=recvonly, and echoed codecs.
func TestBuildAnswerOffersTwoRecvonlyStreams(t *testing.T) {
	ports := []int{30000, 30002}
	answer, err := buildAnswer([]byte(tapOffer), "127.0.0.1", ports)
	if err != nil {
		t.Fatalf("buildAnswer: %v", err)
	}

	var mAudioLines []string
	var recvonlyCount int
	var rtpmapLines []string
	for _, rawLine := range bytes.Split(answer, []byte("\n")) {
		line := strings.TrimRight(string(rawLine), "\r")
		if strings.HasPrefix(line, "m=audio ") {
			mAudioLines = append(mAudioLines, line)
		}
		if line == "a=recvonly" {
			recvonlyCount++
		}
		if strings.HasPrefix(line, "a=rtpmap:") {
			rtpmapLines = append(rtpmapLines, line)
		}
	}

	if len(mAudioLines) != 2 {
		t.Fatalf("want 2 m=audio lines, got %d: %v", len(mAudioLines), mAudioLines)
	}
	if !strings.Contains(mAudioLines[0], "30000") {
		t.Errorf("stream 0: want port 30000, got %q", mAudioLines[0])
	}
	if !strings.Contains(mAudioLines[1], "30002") {
		t.Errorf("stream 1: want port 30002, got %q", mAudioLines[1])
	}
	if recvonlyCount != 2 {
		t.Errorf("want 2 a=recvonly, got %d", recvonlyCount)
	}
	// Codecs echoed: 2 streams × 2 rtpmaps each = 4
	if len(rtpmapLines) != 4 {
		t.Errorf("want 4 a=rtpmap lines (2 per stream), got %d", len(rtpmapLines))
	}
}

// Given a single-stream offer; When parseOffer is called;
// Then one audioStream is returned with the correct formats.
func TestParseOfferExtractsFormats(t *testing.T) {
	sdp := []byte("v=0\r\nm=audio 5004 RTP/AVP 0 8\r\nc=IN IP4 1.2.3.4\r\na=rtpmap:0 PCMU/8000\r\n")
	streams, err := parseOffer(sdp)
	if err != nil {
		t.Fatalf("parseOffer: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("want 1 stream, got %d", len(streams))
	}
	if streams[0].formats != "0 8" {
		t.Errorf("want formats %q, got %q", "0 8", streams[0].formats)
	}
	if len(streams[0].rtpmaps) != 1 {
		t.Errorf("want 1 rtpmap, got %d", len(streams[0].rtpmaps))
	}
}

// Given a SDP with no m=audio; When parseOffer is called; Then an error is returned.
func TestParseOfferErrorOnNoAudio(t *testing.T) {
	sdp := []byte("v=0\r\no=- 0 0 IN IP4 1.2.3.4\r\nm=video 5004 RTP/AVP 96\r\n")
	_, err := parseOffer(sdp)
	if err == nil {
		t.Fatal("want error for SDP with no m=audio, got nil")
	}
}
