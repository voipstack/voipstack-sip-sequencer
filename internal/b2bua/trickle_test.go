package b2bua

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/webrtc/v4"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// ── pure: content-type recognition ──────────────────────────────────────────

func TestIsTrickleContentType(t *testing.T) {
	cases := []struct {
		name string
		ct   string
		want bool
	}{
		{"exact", "application/trickle-ice-sdpfrag", true},
		{"mixed case", "Application/Trickle-ICE-SDPFrag", true},
		{"with params", "application/trickle-ice-sdpfrag; charset=utf-8", true},
		{"surrounding spaces", "  application/trickle-ice-sdpfrag  ", true},
		{"dtmf", "application/dtmf-relay", false},
		{"sdp", "application/sdp", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTrickleContentType(tc.ct); got != tc.want {
				t.Errorf("isTrickleContentType(%q) = %v, want %v", tc.ct, got, tc.want)
			}
		})
	}
}

// ── pure: fragment parsing ───────────────────────────────────────────────────

func TestParseTrickleFragment(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCand []string
		wantEOC  bool
	}{
		{
			name:     "single candidate, no end",
			body:     "a=candidate:1 1 udp 2122 192.0.2.1 5000 typ host\r\n",
			wantCand: []string{"candidate:1 1 udp 2122 192.0.2.1 5000 typ host"},
			wantEOC:  false,
		},
		{
			name: "two candidates plus end-of-candidates",
			body: "m=audio 9 UDP/TLS/RTP/SAVPF 111\r\n" +
				"a=candidate:1 1 udp 2122 192.0.2.1 5000 typ host\r\n" +
				"a=candidate:2 1 udp 1686 198.51.100.2 6000 typ srflx\r\n" +
				"a=end-of-candidates\r\n",
			wantCand: []string{
				"candidate:1 1 udp 2122 192.0.2.1 5000 typ host",
				"candidate:2 1 udp 1686 198.51.100.2 6000 typ srflx",
			},
			wantEOC: true,
		},
		{"end only", "a=end-of-candidates\r\n", nil, true},
		{"lf only line endings", "a=candidate:7 1 udp 2122 192.0.2.9 7000 typ host\n", []string{"candidate:7 1 udp 2122 192.0.2.9 7000 typ host"}, false},
		{"empty body", "", nil, false},
		{"garbage", "not an sdp fragment\r\nv=0\r\n", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frag := parseTrickleFragment([]byte(tc.body))
			if !reflect.DeepEqual(frag.Candidates, tc.wantCand) {
				t.Errorf("candidates = %#v, want %#v", frag.Candidates, tc.wantCand)
			}
			if frag.EndOfCandidates != tc.wantEOC {
				t.Errorf("endOfCandidates = %v, want %v", frag.EndOfCandidates, tc.wantEOC)
			}
		})
	}
}

// ── passthrough: a non-trickle INFO is proxied unchanged ─────────────────────

// An INFO that is not a trickle-ICE fragment (e.g. DTMF, or here a bare INFO with no
// Content-Type) is forwarded to cfg.NextHop by proxyUnmanaged exactly as before.
func TestNonTrickleInfoProxied(t *testing.T) {
	pbx := newFakePBXSimple(t, 200, "OK")
	app := newFakeUAS(t)
	uac := newFakeUACSimple(t)

	engAddr := freeAddr(t)
	_ = startEngine(t, testConfig(engAddr, app.sipURI(), pbx.sipURI()), 0)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := uac.send(ctx, sip.INFO, "sip:"+engAddr)
	if err != nil {
		t.Fatalf("send INFO: %v", err)
	}
	if got := pbx.waitMethod(t, time.Second); got != sip.INFO {
		t.Fatalf("PBX: expected INFO, got %s", got)
	}
	if res.StatusCode != 200 {
		t.Fatalf("UAC: expected 200 relayed from PBX, got %d", res.StatusCode)
	}
}

// ── wiring: a trickle INFO on a secured dialog feeds the leg ─────────────────

// sendTrickleInfo sends an in-dialog INFO carrying a trickle-ICE fragment back to the
// engine over the established dialog.
func sendTrickleInfo(ctx context.Context, sess *sipgo.DialogClientSession, body string) (*sip.Response, error) {
	req := sip.NewRequest(sip.INFO, sess.InviteResponse.Contact().Address)
	req.AppendHeader(sip.NewHeader("Content-Type", trickleICEContentType))
	req.SetBody([]byte(body))
	return sess.Do(ctx, req)
}

// A trickle INFO on a matched secured webphone dialog is consumed (not proxied): every
// a=candidate is fed to the leg via AddRemoteCandidate and a=end-of-candidates via
// EndOfRemoteCandidates, and the INFO is answered 200 OK. Driven through the real
// handler/parse/dialog-match path against the WebRTC library boundary (fake endpoint).
func TestTrickleInfoFeedsSecuredLeg(t *testing.T) {
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	cfg := config.Config{
		SIP:     config.SIP{Listen: listenAddr},
		NextHop: config.NextHop{URI: pbx.sipURI()},
		RTP:     config.RTP{PortRange: "10000-20000"},
		Media:   config.Media{PublicAddress: "203.0.113.7"},
	}
	fakeAnswer := []byte("v=0\r\no=- 0 0 IN IP4 203.0.113.7\r\ns=-\r\nt=0 0\r\n" +
		"a=ice-lite\r\nm=audio 5000 UDP/TLS/RTP/SAVPF 111\r\n")
	fac := &fakeWebRTCFactory{answer: fakeAnswer}

	_ = startEngineOpts(t, cfg, WithWebRTCFactory(fac))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(webrtcOfferSDP))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("wait answer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ack: %v", err)
	}

	body := "a=candidate:1 1 udp 2122 192.0.2.1 5000 typ host\r\n" +
		"a=candidate:2 1 udp 1686 198.51.100.2 6000 typ srflx\r\n" +
		"a=end-of-candidates\r\n"
	res, err := sendTrickleInfo(ctx, sess, body)
	if err != nil {
		t.Fatalf("send trickle INFO: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("trickle INFO answered %d, want 200", res.StatusCode)
	}

	fac.mu.Lock()
	ep := fac.last
	fac.mu.Unlock()
	if ep == nil {
		t.Fatal("no endpoint built")
	}
	cands, eoc := ep.remoteCandidates()
	want := []string{
		"candidate:1 1 udp 2122 192.0.2.1 5000 typ host",
		"candidate:2 1 udp 1686 198.51.100.2 6000 typ srflx",
	}
	if !reflect.DeepEqual(cands, want) {
		t.Errorf("fed candidates = %#v, want %#v", cands, want)
	}
	if !eoc {
		t.Error("end-of-candidates not applied to leg")
	}
}

// ── real pion webphone: trickle brings the leg up (AC1/AC2) ───────────────────

// newTrickleWebphone is a real pion WebRTC client that offers WITHOUT packing its ICE
// candidates into the offer (the candidate-less offer a trickling browser sends). It
// returns the PeerConnection, the candidate-less offer SDP, and a channel that yields
// each gathered candidate string and closes when gathering completes.
func newTrickleWebphone(t *testing.T) (*webrtc.PeerConnection, string, <-chan string) {
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

	cands := make(chan string, 16)
	var once sync.Once
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			once.Do(func() { close(cands) })
			return
		}
		cands <- c.ToJSON().Candidate
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	// Use the offer SDP as produced by CreateOffer — before gathering, so it carries no
	// a=candidate lines. SetLocalDescription starts gathering; candidates arrive on cands.
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local description: %v", err)
	}
	return pc, offer.SDP, cands
}

// AC1/AC2: a real webphone offers without candidates, then delivers its candidate and
// end-of-candidates via in-dialog trickle INFO; the secured leg adds them to its
// connectivity checks and the webphone reaches Connected — DTLS-SRTP completes only
// because the trickled candidate let ICE validate a pair.
func TestRealWebphoneTrickleBringsLegUp(t *testing.T) {
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	wsAddr := freeAddr(t)
	// The engine's ICE-lite answer exceeds the UDP MTU, so the webphone signals over
	// WebSocket (as browsers do). The trickle INFOs route back to the engine's UDP
	// Contact and stay small. No application sequence: this targets the media leg.
	cfg := config.Config{
		SIP:     config.SIP{Listen: freeAddr(t)},
		WS:      config.WS{Listen: wsAddr},
		NextHop: config.NextHop{URI: pbx.sipURI()},
		RTP:     config.RTP{PortRange: "10000-20000"},
		Media:   config.Media{PublicAddress: "127.0.0.1"}, // reachable by the local client
	}
	eng := startEngineWS(t, cfg) // real pion factory (default)

	pc, offerSDP, cands := newTrickleWebphone(t)

	var once sync.Once
	connected := make(chan struct{})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			once.Do(func() { close(connected) })
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := caller.invite(ctx, "sip:"+wsAddr+";transport=ws", []byte(offerSDP))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("wait answer: %v", err)
	}
	answer := string(sess.InviteResponse.Body())
	// No SIP ACK: in-dialog routing back to the engine over ws follows the Contact;
	// the DTLS-SRTP/ICE handshake is media-level and independent of the ACK.
	// The webphone learns the anchor's host candidate from the answer, then trickles its
	// own candidates back over INFO.
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: answer,
	}); err != nil {
		t.Fatalf("set answer on webphone: %v", err)
	}

	go func() {
		for c := range cands {
			if _, err := sendTrickleInfo(ctx, sess, "a="+c+"\r\n"); err != nil {
				return
			}
		}
		_, _ = sendTrickleInfo(ctx, sess, "a=end-of-candidates\r\n")
	}()

	select {
	case <-connected:
	case <-time.After(20 * time.Second):
		t.Fatal("webphone did not reach Connected: trickled candidate did not establish media")
	}

	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call, got %d", n)
	}
}
