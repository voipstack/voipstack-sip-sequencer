package b2bua

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// spyMetrics records MediaCodecMismatch calls; all other sink methods are no-ops.
type spyMetrics struct {
	noopMetrics
	mu         sync.Mutex
	mismatches [][2]string
}

func (s *spyMetrics) MediaCodecMismatch(endpointCodec, pbxCodec string) {
	s.mu.Lock()
	s.mismatches = append(s.mismatches, [2]string{endpointCodec, pbxCodec})
	s.mu.Unlock()
}

func (s *spyMetrics) mismatchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.mismatches)
}

// lockedBuffer is a concurrency-safe io.Writer for capturing slog output emitted on the
// bridge goroutine while the test reads it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLogs redirects the default slog logger to a buffer for the test's duration.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// bridgeWebphone drives a webphone (WebRTC) INVITE bridged to a plain-RTP PBX: the
// secured leg answers with webrtcAnswer, the PBX answers with pbxAnswer. It blocks until
// the caller is answered and ACKed, so the codec check (run synchronously before the 200
// OK) has already fired by the time it returns. It returns the X-Sequencer-Call-Id the
// PBX leg carried.
func bridgeWebphone(t *testing.T, webrtcAnswer, pbxAnswer []byte, opts ...Option) string {
	t.Helper()
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	cfg := config.Config{
		SIP:     config.SIP{Listen: listenAddr},
		NextHop: config.NextHop{URI: pbx.sipURI()},
		RTP:     config.RTP{PortRange: "10000-20000"},
		Media:   config.Media{PublicAddress: "203.0.113.7"},
	}
	fac := &fakeWebRTCFactory{answer: webrtcAnswer}
	allOpts := append([]Option{WithWebRTCFactory(fac)}, opts...)
	startEngineOpts(t, cfg, allOpts...)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	var callID string
	pbxDone := make(chan struct{})
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		if h := dss.InviteRequest.GetHeader("X-Sequencer-Call-Id"); h != nil {
			callID = h.Value()
		}
		_ = dss.Respond(200, "OK", pbxAnswer)
		close(pbxDone)
	}()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(webrtcOfferSDP))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("wait answer: %v", err)
	}
	<-pbxDone
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ack: %v", err)
	}
	return callID
}

// A secured-leg answer carrying opus (pt 111). selectedAudioCodec needs only the
// m=audio line and its a=rtpmap.
const webrtcOpusAnswer = "v=0\r\no=- 0 0 IN IP4 203.0.113.7\r\ns=-\r\nt=0 0\r\n" +
	"a=ice-lite\r\nm=audio 5000 UDP/TLS/RTP/SAVPF 111\r\na=rtpmap:111 opus/48000/2\r\n"

// Given a webphone leg negotiating opus and a PBX leg negotiating G722; When the call is
// bridged; Then one ERROR log names both codecs and the call id, and the mismatch metric
// is incremented.
func TestCodecMismatchLoggedAndCounted(t *testing.T) {
	logs := captureLogs(t)
	spy := &spyMetrics{}

	// PBX answers G722 (static pt 9, no a=rtpmap).
	pbxG722 := []byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 9 RTP/AVP 9\r\n")

	callID := bridgeWebphone(t, []byte(webrtcOpusAnswer), pbxG722, WithMetrics(spy))

	if got := spy.mismatchCount(); got != 1 {
		t.Fatalf("MediaCodecMismatch called %d times, want 1", got)
	}

	out := logs.String()
	if !strings.Contains(out, "media codec mismatch") {
		t.Fatalf("expected codec-mismatch ERROR log, got:\n%s", out)
	}
	for _, want := range []string{"opus", "G722", callID} {
		if want == "" || !strings.Contains(out, want) {
			t.Errorf("mismatch log missing %q:\n%s", want, out)
		}
	}
}

// Given a webphone leg and a PBX leg that both negotiate opus (different payload type
// numbers); When the call is bridged; Then no mismatch is logged or counted.
func TestCodecMatchQuiet(t *testing.T) {
	logs := captureLogs(t)
	spy := &spyMetrics{}

	// PBX answers opus too, but numbers it pt 96 — payload type differs, codec matches.
	pbxOpus := []byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 9 RTP/AVP 96\r\na=rtpmap:96 opus/48000/2\r\n")

	bridgeWebphone(t, []byte(webrtcOpusAnswer), pbxOpus, WithMetrics(spy))

	if got := spy.mismatchCount(); got != 0 {
		t.Fatalf("MediaCodecMismatch called %d times, want 0", got)
	}
	if out := logs.String(); strings.Contains(out, "media codec mismatch") {
		t.Fatalf("matching codecs must not log a mismatch:\n%s", out)
	}
}
