package b2bua

import (
	"testing"
)

// ── parsePortRange ────────────────────────────────────────────────────────────

func TestParsePortRange(t *testing.T) {
	tests := []struct {
		input   string
		wantMin int
		wantMax int
		wantErr bool
	}{
		{input: "10000-20000", wantMin: 10000, wantMax: 20000},
		{input: "2-4", wantMin: 2, wantMax: 4},
		{input: "10000-10002", wantMin: 10000, wantMax: 10002},
		// error cases
		{input: "", wantErr: true},
		{input: "10000", wantErr: true},
		{input: "abc-20000", wantErr: true},
		{input: "10000-abc", wantErr: true},
		{input: "10001-20000", wantErr: true}, // odd min
		{input: "20000-10000", wantErr: true}, // min >= max
		{input: "0-10000", wantErr: true},     // min not positive
		{input: "-10000-20000", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			min, max, err := parsePortRange(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got min=%d max=%d", tc.input, min, max)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if min != tc.wantMin || max != tc.wantMax {
				t.Fatalf("got (%d,%d), want (%d,%d)", min, max, tc.wantMin, tc.wantMax)
			}
		})
	}
}

// ── parseMedia ────────────────────────────────────────────────────────────────

func TestParseMedia(t *testing.T) {
	tests := []struct {
		name     string
		sdp      string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{
			name:     "basic session-level c=",
			sdp:      "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 192.0.2.1\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0\r\n",
			wantHost: "192.0.2.1",
			wantPort: 5004,
		},
		{
			name:     "media-level c= overrides session",
			sdp:      "v=0\r\nc=IN IP4 192.0.2.1\r\nm=audio 5004 RTP/AVP 0\r\nc=IN IP4 10.0.0.1\r\n",
			wantHost: "10.0.0.1",
			wantPort: 5004,
		},
		{
			name:     "existing testSDP",
			sdp:      testSDP,
			wantHost: "127.0.0.1",
			wantPort: 9,
		},
		{
			name:    "no audio m= line",
			sdp:     "v=0\r\nc=IN IP4 192.0.2.1\r\nm=video 5004 RTP/AVP 96\r\n",
			wantErr: true,
		},
		{
			name:    "no c= line",
			sdp:     "v=0\r\nm=audio 5004 RTP/AVP 0\r\n",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			host, port, err := parseMedia([]byte(tc.sdp))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got host=%q port=%d", host, port)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tc.wantHost {
				t.Fatalf("host = %q, want %q", host, tc.wantHost)
			}
			if port != tc.wantPort {
				t.Fatalf("port = %d, want %d", port, tc.wantPort)
			}
		})
	}
}

// ── rewriteToAnchor ───────────────────────────────────────────────────────────

func TestRewriteToAnchor(t *testing.T) {
	const input = "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 192.0.2.1\r\nt=0 0\r\nm=audio 5004 RTP/AVP 0 8\r\na=rtpmap:0 PCMU/8000\r\n"
	const wantHost = "10.1.2.3"
	const wantPort = 12000

	out, err := rewriteToAnchor([]byte(input), wantHost, wantPort)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// host rewritten in c= line
	host, port, err := parseMedia(out)
	if err != nil {
		t.Fatalf("parseMedia on rewritten SDP: %v", err)
	}
	if host != wantHost {
		t.Fatalf("c= host = %q, want %q", host, wantHost)
	}
	if port != wantPort {
		t.Fatalf("m= port = %d, want %d", port, wantPort)
	}

	// codec attribute must be preserved verbatim
	outStr := string(out)
	if !contains(outStr, "a=rtpmap:0 PCMU/8000") {
		t.Fatalf("codec attribute not preserved in rewritten SDP:\n%s", outStr)
	}

	// o= line must be unchanged
	if !contains(outStr, "o=- 0 0 IN IP4 127.0.0.1") {
		t.Fatalf("o= line changed unexpectedly:\n%s", outStr)
	}
}

func TestRewriteToAnchorPreservesCRLF(t *testing.T) {
	input := "v=0\r\nc=IN IP4 1.2.3.4\r\nm=audio 1000 RTP/AVP 0\r\n"
	out, err := rewriteToAnchor([]byte(input), "5.6.7.8", 2000)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Every line that existed before must still end in \r\n
	for _, line := range []string{"v=0\r", "c=IN IP4 5.6.7.8\r", "m=audio 2000 RTP/AVP 0\r"} {
		if !contains(string(out), line) {
			t.Fatalf("expected %q in output:\n%s", line, out)
		}
	}
}

func TestRewriteToAnchorErrors(t *testing.T) {
	valid := []byte("v=0\r\nc=IN IP4 1.2.3.4\r\nm=audio 1000 RTP/AVP 0\r\n")
	if _, err := rewriteToAnchor(valid, "", 1000); err == nil {
		t.Fatal("expected error for empty host")
	}
	if _, err := rewriteToAnchor(valid, "1.2.3.4", 0); err == nil {
		t.Fatal("expected error for zero port")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ── extractAudioCodecs ────────────────────────────────────────────────────────

func TestExtractAudioCodecs(t *testing.T) {
	tests := []struct {
		name        string
		sdp         string
		wantFormats string
		wantRtpmaps []string
		wantFmtps   []string
		wantErr     bool
	}{
		{
			name:        "single codec no attrs",
			sdp:         "v=0\r\nc=IN IP4 1.2.3.4\r\nm=audio 5004 RTP/AVP 0\r\n",
			wantFormats: "0",
		},
		{
			name:        "multiple codecs with rtpmap",
			sdp:         "v=0\r\nc=IN IP4 1.2.3.4\r\nm=audio 5004 RTP/AVP 0 8 101\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:101 telephone-event/8000\r\na=fmtp:101 0-15\r\n",
			wantFormats: "0 8 101",
			wantRtpmaps: []string{"a=rtpmap:0 PCMU/8000", "a=rtpmap:8 PCMA/8000", "a=rtpmap:101 telephone-event/8000"},
			wantFmtps:   []string{"a=fmtp:101 0-15"},
		},
		{
			name:    "no audio m= line",
			sdp:     "v=0\r\nc=IN IP4 1.2.3.4\r\nm=video 5004 RTP/AVP 96\r\n",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			formats, rtpmaps, fmtps, err := extractAudioCodecs([]byte(tc.sdp))
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if formats != tc.wantFormats {
				t.Errorf("formats = %q, want %q", formats, tc.wantFormats)
			}
			if len(rtpmaps) != len(tc.wantRtpmaps) {
				t.Errorf("rtpmaps = %v, want %v", rtpmaps, tc.wantRtpmaps)
			}
			if len(fmtps) != len(tc.wantFmtps) {
				t.Errorf("fmtps = %v, want %v", fmtps, tc.wantFmtps)
			}
		})
	}
}

// ── selectedAudioCodec ────────────────────────────────────────────────────────

func TestSelectedAudioCodec(t *testing.T) {
	tests := []struct {
		name     string
		sdp      string
		wantPT   int
		wantName string
		wantRate int
		wantErr  bool
	}{
		{
			name:     "opus dynamic with rtpmap",
			sdp:      "v=0\r\nc=IN IP4 1.2.3.4\r\nm=audio 5000 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 opus/48000/2\r\n",
			wantPT:   111,
			wantName: "opus",
			wantRate: 48000,
		},
		{
			name:     "G722 static no rtpmap falls back to table",
			sdp:      "v=0\r\nc=IN IP4 1.2.3.4\r\nm=audio 5004 RTP/AVP 9\r\n",
			wantPT:   9,
			wantName: "G722",
			wantRate: 8000,
		},
		{
			name:     "PCMU static no rtpmap falls back to table",
			sdp:      "v=0\r\nc=IN IP4 1.2.3.4\r\nm=audio 5004 RTP/AVP 0\r\n",
			wantPT:   0,
			wantName: "PCMU",
			wantRate: 8000,
		},
		{
			name:     "first format is the selected one",
			sdp:      "v=0\r\nc=IN IP4 1.2.3.4\r\nm=audio 5004 RTP/AVP 8 0 101\r\na=rtpmap:8 PCMA/8000\r\na=rtpmap:101 telephone-event/8000\r\n",
			wantPT:   8,
			wantName: "PCMA",
			wantRate: 8000,
		},
		{
			name:    "no audio m= line",
			sdp:     "v=0\r\nc=IN IP4 1.2.3.4\r\nm=video 5004 RTP/AVP 96\r\n",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectedAudioCodec([]byte(tc.sdp))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.PayloadType != tc.wantPT || got.EncodingName != tc.wantName || got.ClockRate != tc.wantRate {
				t.Fatalf("got %+v, want pt=%d name=%q rate=%d", got, tc.wantPT, tc.wantName, tc.wantRate)
			}
		})
	}
}

func TestCodecsMatch(t *testing.T) {
	opus111 := AudioCodec{PayloadType: 111, EncodingName: "opus", ClockRate: 48000}
	opus96 := AudioCodec{PayloadType: 96, EncodingName: "OPUS", ClockRate: 48000}
	g722 := AudioCodec{PayloadType: 9, EncodingName: "G722", ClockRate: 8000}
	unknown96 := AudioCodec{PayloadType: 96}
	unknown97 := AudioCodec{PayloadType: 97}

	if !codecsMatch(opus111, opus96) {
		t.Error("same codec, different payload type and case should match")
	}
	if codecsMatch(opus111, g722) {
		t.Error("opus vs G722 must not match")
	}
	if !codecsMatch(unknown96, unknown96) {
		t.Error("identical unknown payload type should match")
	}
	if codecsMatch(unknown96, unknown97) {
		t.Error("different unknown payload types must not match")
	}
}

// ── buildTapOffer ─────────────────────────────────────────────────────────────

func TestBuildTapOffer(t *testing.T) {
	const callOffer = "v=0\r\nc=IN IP4 192.0.2.1\r\nm=audio 5000 RTP/AVP 0 8\r\na=rtpmap:0 PCMU/8000\r\na=rtpmap:8 PCMA/8000\r\na=sendrecv\r\n"

	out, err := buildTapOffer([]byte(callOffer), "10.0.0.1", 6000, 6002)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := string(out)

	// Two m=audio blocks at the allocated ports.
	if !contains(s, "m=audio 6000 RTP/AVP 0 8") {
		t.Errorf("caller stream port 6000 not found:\n%s", s)
	}
	if !contains(s, "m=audio 6002 RTP/AVP 0 8") {
		t.Errorf("callee stream port 6002 not found:\n%s", s)
	}

	// Each block has a=recvonly.
	count := 0
	for i := 0; i <= len(s)-len("a=recvonly"); i++ {
		if s[i:i+len("a=recvonly")] == "a=recvonly" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("expected 2 a=recvonly, got %d:\n%s", count, s)
	}

	// Codec lines preserved verbatim.
	if !contains(s, "a=rtpmap:0 PCMU/8000") {
		t.Errorf("rtpmap:0 not preserved:\n%s", s)
	}
	if !contains(s, "a=rtpmap:8 PCMA/8000") {
		t.Errorf("rtpmap:8 not preserved:\n%s", s)
	}

	// Host rewritten.
	if !contains(s, "c=IN IP4 10.0.0.1") {
		t.Errorf("c= host not set:\n%s", s)
	}

	// No a=sendrecv from original (not copied — only rtpmap/fmtp are copied).
	if contains(s, "a=sendrecv") {
		t.Errorf("a=sendrecv should not appear in tap offer:\n%s", s)
	}
}

func TestBuildTapOfferNoAudioError(t *testing.T) {
	const noAudio = "v=0\r\nc=IN IP4 1.2.3.4\r\nm=video 5004 RTP/AVP 96\r\n"
	_, err := buildTapOffer([]byte(noAudio), "10.0.0.1", 6000, 6002)
	if err == nil {
		t.Fatal("expected error for SDP without audio m= line")
	}
}

// ── buildInactiveOffer ────────────────────────────────────────────────────────

func TestBuildInactiveOffer(t *testing.T) {
	const callOffer = "v=0\r\nc=IN IP4 192.0.2.1\r\nm=audio 5000 RTP/AVP 0 8\r\na=rtpmap:0 PCMU/8000\r\na=fmtp:101 0-15\r\n"

	out, err := buildInactiveOffer([]byte(callOffer), "10.0.0.1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := string(out)

	if !contains(s, "m=audio 0 RTP/AVP 0 8") {
		t.Errorf("inactive m=audio not found:\n%s", s)
	}
	if !contains(s, "a=inactive") {
		t.Errorf("a=inactive not found:\n%s", s)
	}
	if !contains(s, "a=rtpmap:0 PCMU/8000") {
		t.Errorf("rtpmap not preserved:\n%s", s)
	}
	if !contains(s, "a=fmtp:101 0-15") {
		t.Errorf("fmtp not preserved:\n%s", s)
	}
}

// ── parseTapAnswer ────────────────────────────────────────────────────────────

func TestParseTapAnswer(t *testing.T) {
	tests := []struct {
		name    string
		sdp     string
		wantH1  string
		wantP1  int
		wantH2  string
		wantP2  int
		wantErr bool
	}{
		{
			name:   "two streams session-level c=",
			sdp:    "v=0\r\nc=IN IP4 10.0.0.1\r\nm=audio 7000 RTP/AVP 0\r\nm=audio 7002 RTP/AVP 0\r\n",
			wantH1: "10.0.0.1", wantP1: 7000,
			wantH2: "10.0.0.1", wantP2: 7002,
		},
		{
			name:   "media-level c= overrides",
			sdp:    "v=0\r\nc=IN IP4 10.0.0.1\r\nm=audio 7000 RTP/AVP 0\r\nc=IN IP4 10.0.0.2\r\nm=audio 7002 RTP/AVP 0\r\nc=IN IP4 10.0.0.3\r\n",
			wantH1: "10.0.0.2", wantP1: 7000,
			wantH2: "10.0.0.3", wantP2: 7002,
		},
		{
			name:   "stream1 port 0 tolerated",
			sdp:    "v=0\r\nc=IN IP4 10.0.0.1\r\nm=audio 0 RTP/AVP 0\r\nm=audio 7002 RTP/AVP 0\r\n",
			wantH1: "10.0.0.1", wantP1: 0,
			wantH2: "10.0.0.1", wantP2: 7002,
		},
		{
			name:    "only one stream",
			sdp:     "v=0\r\nc=IN IP4 10.0.0.1\r\nm=audio 7000 RTP/AVP 0\r\n",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h1, p1, h2, p2, err := parseTapAnswer([]byte(tc.sdp))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got h1=%s p1=%d h2=%s p2=%d", h1, p1, h2, p2)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if h1 != tc.wantH1 || p1 != tc.wantP1 {
				t.Errorf("stream1 = %s:%d, want %s:%d", h1, p1, tc.wantH1, tc.wantP1)
			}
			if h2 != tc.wantH2 || p2 != tc.wantP2 {
				t.Errorf("stream2 = %s:%d, want %s:%d", h2, p2, tc.wantH2, tc.wantP2)
			}
		})
	}
}
