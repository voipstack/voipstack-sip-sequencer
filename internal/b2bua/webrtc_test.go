package b2bua

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/pion/webrtc/v4"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// A minimal browser-style WebRTC offer: secure profile (RTP/SAVPF) plus a DTLS
// fingerprint and rtcp-mux, as jssip/sip.js produce.
const webrtcOfferSDP = "v=0\r\n" +
	"o=- 0 0 IN IP4 127.0.0.1\r\n" +
	"s=-\r\n" +
	"t=0 0\r\n" +
	"m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
	"c=IN IP4 127.0.0.1\r\n" +
	"a=rtpmap:111 opus/48000/2\r\n" +
	"a=fingerprint:sha-256 AA:BB:CC\r\n" +
	"a=rtcp-mux\r\n"

// ── pure predicate (offer-type detection) ────────────────────────────────────

// offerIsWebRTC must distinguish a WebRTC offer from a plain RTP/AVP one so the plain
// anchoring path is untouched.
func TestOfferIsWebRTC(t *testing.T) {
	cases := []struct {
		name string
		sdp  string
		want bool
	}{
		{"plain rtp/avp", testSDP, false},
		{"savpf profile", webrtcOfferSDP, true},
		{"fingerprint only", "v=0\r\nm=audio 9 RTP/AVP 0\r\na=fingerprint:sha-256 AA\r\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := offerIsWebRTC([]byte(tc.sdp)); got != tc.want {
				t.Errorf("offerIsWebRTC = %v, want %v", got, tc.want)
			}
		})
	}
}

// ── per-leg media security (AC5) ─────────────────────────────────────────────

// Each leg reports its own security independently; nothing hardcodes "webphone = SRTP,
// other = RTP".
func TestPerLegSecurityIsIndependent(t *testing.T) {
	plain := &AnchorSide{}
	if got := plain.Security(); got != SecurityPlainRTP {
		t.Errorf("AnchorSide.Security() = %v, want %v", got, SecurityPlainRTP)
	}

	secured := &SecuredLeg{endpoint: &fakeWebRTCEndpoint{}}
	if got := secured.Security(); got != SecurityDTLSSRTP {
		t.Errorf("SecuredLeg.Security() = %v, want %v", got, SecurityDTLSSRTP)
	}

	// Both satisfy MediaLeg.
	var _ MediaLeg = plain
	var _ MediaLeg = secured
}

// ── fake WebRTC boundary (deterministic engine-wiring tests) ─────────────────

type fakeWebRTCEndpoint struct {
	answer     []byte
	mu         sync.Mutex
	closed     bool
	candidates []string
	gotEOC     bool
}

func (f *fakeWebRTCEndpoint) Answer(offer []byte) ([]byte, error) { return f.answer, nil }
func (f *fakeWebRTCEndpoint) ReadRTP(buf []byte) (int, error)     { return 0, errors.New("no media") }
func (f *fakeWebRTCEndpoint) LocalPort() int                      { return 0 }
func (f *fakeWebRTCEndpoint) AddRemoteCandidate(candidate string) error {
	f.mu.Lock()
	f.candidates = append(f.candidates, candidate)
	f.mu.Unlock()
	return nil
}
func (f *fakeWebRTCEndpoint) EndOfRemoteCandidates() error {
	f.mu.Lock()
	f.gotEOC = true
	f.mu.Unlock()
	return nil
}
func (f *fakeWebRTCEndpoint) remoteCandidates() ([]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.candidates))
	copy(out, f.candidates)
	return out, f.gotEOC
}
func (f *fakeWebRTCEndpoint) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}
func (f *fakeWebRTCEndpoint) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

type fakeWebRTCFactory struct {
	answer        []byte
	mu            sync.Mutex
	gotPublicAddr string
	last          *fakeWebRTCEndpoint
}

func (f *fakeWebRTCFactory) NewEndpoint(publicAddr string) (WebRTCEndpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotPublicAddr = publicAddr
	ep := &fakeWebRTCEndpoint{answer: f.answer}
	f.last = ep
	return ep, nil
}

// startEngineOpts starts the engine with construction options (e.g. a fake factory),
// mirroring startEngine's ready-wait + cleanup.
func startEngineOpts(t *testing.T, cfg config.Config, opts ...Option) *Engine {
	t.Helper()
	eng, err := New(cfg, opts...)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	ready := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		rctx := context.WithValue(ctx, sipgo.ListenReadyCtxKey,
			sipgo.ListenReadyFuncCtxValue(func(_, _ string) { close(ready) }))
		_ = eng.Run(rctx)
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("engine did not start in time")
	}
	t.Cleanup(func() {
		cancel()
		_ = eng.Shutdown()
	})
	return eng
}

// A WebRTC INVITE is answered from the secured leg's SDP, the configured public
// address reaches the factory, and no PBX leg is dialed (bringing the leg up is the
// whole of STORY-001-019; bridging to a PBX is STORY-001-021).
func TestWebRTCOfferAnsweredFromSecuredLegNoPBX(t *testing.T) {
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	// No application sequence: this test targets the media leg, not the app chain.
	cfg := config.Config{
		SIP:     config.SIP{Listen: listenAddr},
		NextHop: config.NextHop{URI: pbx.sipURI()},
		RTP:     config.RTP{PortRange: "10000-20000"},
		Media:   config.Media{PublicAddress: "203.0.113.7"},
	}

	fakeAnswer := []byte("v=0\r\no=- 0 0 IN IP4 203.0.113.7\r\ns=-\r\nt=0 0\r\n" +
		"a=ice-lite\r\nm=audio 5000 UDP/TLS/RTP/SAVPF 111\r\n")
	fac := &fakeWebRTCFactory{answer: fakeAnswer}

	eng := startEngineOpts(t, cfg, WithWebRTCFactory(fac))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(webrtcOfferSDP))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("wait answer: %v", err)
	}
	if got := string(sess.InviteResponse.Body()); got != string(fakeAnswer) {
		t.Errorf("answer body = %q, want secured leg answer %q", got, string(fakeAnswer))
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ack: %v", err)
	}

	fac.mu.Lock()
	gotAddr := fac.gotPublicAddr
	fac.mu.Unlock()
	if gotAddr != "203.0.113.7" {
		t.Errorf("factory public address = %q, want %q", gotAddr, "203.0.113.7")
	}

	// No PBX leg is dialed for a webphone leg in this story.
	pbx.noInvite(t, 300*time.Millisecond)

	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call, got %d", n)
	}
}

// ── real pion webphone (no internal mocks; AGENTS.md real fake) ──────────────

// newPionWebphone is a real pion-backed WebRTC client (a stand-in for a browser):
// it offers an audio track and runs full ICE. It returns the PeerConnection and its
// (gathering-complete) offer SDP.
func newPionWebphone(t *testing.T) (*webrtc.PeerConnection, string) {
	t.Helper()
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("register codecs: %v", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("webphone peer connection: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "webphone")
	if err != nil {
		t.Fatalf("local track: %v", err)
	}
	if _, err := pc.AddTrack(track); err != nil {
		t.Fatalf("add track: %v", err)
	}

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	gather := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local description: %v", err)
	}
	<-gather
	return pc, pc.LocalDescription().SDP
}

// AC1/AC2/AC3/AC4/AC6: a real WebRTC client offers media; the sequencer answers
// ICE-lite (host candidate on the configured public address, no relay/srflx), and the
// DTLS-SRTP handshake completes — the client's PeerConnection reaches Connected, which
// requires the anchor to have validated ICE checks itself and terminated DTLS.
func TestWebRTCRealHandshakeBringsLegUp(t *testing.T) {
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	wsAddr := freeAddr(t)
	// A real WebRTC offer carries full ICE candidates and exceeds the UDP MTU, so the
	// webphone signals over WebSocket (as browsers do). No application sequence: this
	// test targets the media leg.
	cfg := config.Config{
		SIP:     config.SIP{Listen: freeAddr(t)},
		WS:      config.WS{Listen: wsAddr},
		NextHop: config.NextHop{URI: pbx.sipURI()},
		RTP:     config.RTP{PortRange: "10000-20000"},
		Media:   config.Media{PublicAddress: "127.0.0.1"}, // reachable by the local client
	}
	eng := startEngineWS(t, cfg)

	pc, offerSDP := newPionWebphone(t)

	var once sync.Once
	connected := make(chan struct{})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sess, err := caller.invite(ctx, "sip:"+wsAddr+";transport=ws", []byte(offerSDP))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("wait answer: %v", err)
	}
	answer := string(sess.InviteResponse.Body())
	// No SIP ACK: in-dialog routing back to the engine over ws follows the Contact
	// header (per-transport rewriting is out of scope). The DTLS-SRTP/ICE handshake is
	// media-level and independent of the ACK.

	// AC1/AC3: ICE-lite answer shape.
	for _, want := range []string{"a=ice-lite", "a=fingerprint:", "a=rtcp-mux"} {
		if !strings.Contains(answer, want) {
			t.Errorf("answer missing %q:\n%s", want, answer)
		}
	}
	// AC1: no TURN/relay or srflx candidate.
	for _, forbidden := range []string{"typ relay", "typ srflx"} {
		if strings.Contains(answer, forbidden) {
			t.Errorf("answer must not advertise %q:\n%s", forbidden, answer)
		}
	}
	// AC6: the advertised host candidate carries the configured public address.
	if !hasHostCandidate(answer, "127.0.0.1") {
		t.Errorf("answer has no host candidate on the public address:\n%s", answer)
	}

	// AC2/AC4: applying the answer drives ICE + DTLS to completion.
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: answer,
	}); err != nil {
		t.Fatalf("set answer on webphone: %v", err)
	}
	select {
	case <-connected:
	case <-time.After(15 * time.Second):
		t.Fatal("webphone did not reach Connected: ICE/DTLS-SRTP did not complete")
	}

	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call, got %d", n)
	}
}

// hasHostCandidate reports whether the SDP advertises an ICE host candidate carrying
// the given address.
func hasHostCandidate(sdp, addr string) bool {
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.Contains(line, "candidate") && strings.Contains(line, "typ host") &&
			strings.Contains(line, " "+addr+" ") {
			return true
		}
	}
	return false
}
