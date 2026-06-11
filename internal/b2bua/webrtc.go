package b2bua

import (
	"fmt"
	"net"
	"sync"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

// WebRTCEndpoint is the media-library boundary: a terminated WebRTC media endpoint
// defined by its behavior (answer a webphone offer with ICE-lite + DTLS-SRTP, then
// expose plaintext RTP), not by any library's API. The concrete pion implementation
// is the only thing that imports a WebRTC library; everything else depends on this
// interface, so the library is swappable.
type WebRTCEndpoint interface {
	// Answer terminates the offer: it answers ICE-lite, gathers a host candidate,
	// drives the DTLS-SRTP handshake setup, and returns the SDP answer to send back.
	Answer(offer []byte) (answer []byte, err error)
	// ReadRTP yields one decrypted RTP packet from the endpoint (the seam the media
	// relay — STORY-001-021 — will consume).
	ReadRTP(buf []byte) (int, error)
	// LocalPort is the UDP port the endpoint listens on (RTP and RTCP share it).
	LocalPort() int
	// Close releases the endpoint's sockets and goroutines; idempotent.
	Close() error
}

// WebRTCFactory builds a WebRTCEndpoint. It is held by the Engine and injected in
// tests with a fake, keeping the secured-leg wiring testable without pion.
type WebRTCFactory interface {
	NewEndpoint(publicAddr string) (WebRTCEndpoint, error)
}

// SecuredLeg is the DTLS-SRTP media leg. It satisfies MediaLeg, delegating to a
// WebRTCEndpoint, and carries the SDP answer to return to the webphone.
type SecuredLeg struct {
	endpoint  WebRTCEndpoint
	answerSDP []byte
}

// newSecuredLeg builds the endpoint and brings the leg up by answering the offer.
// On answer failure it closes the endpoint so no socket or goroutine leaks.
func newSecuredLeg(f WebRTCFactory, publicAddr string, offer []byte) (*SecuredLeg, error) {
	ep, err := f.NewEndpoint(publicAddr)
	if err != nil {
		return nil, fmt.Errorf("create webrtc endpoint: %w", err)
	}
	ans, err := ep.Answer(offer)
	if err != nil {
		_ = ep.Close()
		return nil, fmt.Errorf("webrtc answer: %w", err)
	}
	return &SecuredLeg{endpoint: ep, answerSDP: ans}, nil
}

// Security reports that a secured leg is DTLS-SRTP.
func (l *SecuredLeg) Security() MediaSecurity { return SecurityDTLSSRTP }

// ReadRTP yields one decrypted RTP packet from the endpoint.
func (l *SecuredLeg) ReadRTP(buf []byte) (int, error) { return l.endpoint.ReadRTP(buf) }

// AnswerSDP returns the ICE-lite/DTLS-SRTP SDP answer to send back to the webphone.
func (l *SecuredLeg) AnswerSDP() []byte { return l.answerSDP }

// Close tears down the endpoint (idempotent).
func (l *SecuredLeg) Close() { _ = l.endpoint.Close() }

// pionFactory is the production WebRTCFactory, backed by pion/webrtc.
type pionFactory struct{}

// NewEndpoint builds an ICE-lite pion PeerConnection: a single UDP mux (so RTP and
// RTCP share one port — rtcp-mux), a lite ICE agent that validates but never
// initiates connectivity checks, and — when publicAddr is set — a 1:1 NAT mapping so
// the gathered host candidate carries the configured public address.
func (pionFactory) NewEndpoint(publicAddr string) (WebRTCEndpoint, error) {
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("bind ice udp mux: %w", err)
	}

	se := webrtc.SettingEngine{}
	se.SetLite(true)
	if publicAddr != "" {
		se.SetNAT1To1IPs([]string{publicAddr}, webrtc.ICECandidateTypeHost)
	}
	mux := webrtc.NewICEUDPMux(logging.NewDefaultLoggerFactory().NewLogger("ice"), udpConn)
	se.SetICEUDPMux(mux)

	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		_ = mux.Close()
		_ = udpConn.Close()
		return nil, fmt.Errorf("register codecs: %w", err)
	}

	api := webrtc.NewAPI(webrtc.WithSettingEngine(se), webrtc.WithMediaEngine(me))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		_ = mux.Close()
		_ = udpConn.Close()
		return nil, fmt.Errorf("create peer connection: %w", err)
	}

	return &pionEndpoint{
		pc:      pc,
		mux:     mux,
		udpConn: udpConn,
		port:    udpConn.LocalAddr().(*net.UDPAddr).Port,
		ready:   make(chan struct{}),
	}, nil
}

// pionEndpoint terminates one webphone's WebRTC media. It is the only type that
// touches pion; the rest of the package sees it as a WebRTCEndpoint.
type pionEndpoint struct {
	pc      *webrtc.PeerConnection
	mux     ice.UDPMux
	udpConn *net.UDPConn
	port    int

	ready     chan struct{} // closed when the remote track arrives or on Close
	readyOnce sync.Once     // guards close(ready)
	mu        sync.Mutex    // guards track
	track     *webrtc.TrackRemote
	closeOnce sync.Once
}

// Answer drives ICE-lite gathering and the DTLS-SRTP setup, returning the SDP answer.
// rtcp-mux and the DTLS fingerprint are produced by pion in the answer. The OnTrack
// handler captures the inbound plaintext RTP track for ReadRTP.
func (e *pionEndpoint) Answer(offer []byte) ([]byte, error) {
	e.pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		e.mu.Lock()
		e.track = tr
		e.mu.Unlock()
		e.readyOnce.Do(func() { close(e.ready) })
	})

	if err := e.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(offer),
	}); err != nil {
		return nil, fmt.Errorf("set remote description: %w", err)
	}
	answer, err := e.pc.CreateAnswer(nil)
	if err != nil {
		return nil, fmt.Errorf("create answer: %w", err)
	}
	gather := webrtc.GatheringCompletePromise(e.pc)
	if err := e.pc.SetLocalDescription(answer); err != nil {
		return nil, fmt.Errorf("set local description: %w", err)
	}
	<-gather
	local := e.pc.LocalDescription()
	if local == nil {
		return nil, fmt.Errorf("no local description after ice gathering")
	}
	return []byte(local.SDP), nil
}

// ReadRTP blocks until the remote track is available, then reads one decrypted RTP
// packet and copies its wire bytes into buf. A Close before the track arrives unblocks
// and returns an error rather than hanging.
func (e *pionEndpoint) ReadRTP(buf []byte) (int, error) {
	<-e.ready
	e.mu.Lock()
	tr := e.track
	e.mu.Unlock()
	if tr == nil {
		return 0, fmt.Errorf("webrtc endpoint closed before track ready")
	}
	pkt, _, err := tr.ReadRTP()
	if err != nil {
		return 0, err
	}
	raw, err := pkt.Marshal()
	if err != nil {
		return 0, fmt.Errorf("marshal rtp: %w", err)
	}
	return copy(buf, raw), nil
}

// LocalPort returns the shared RTP/RTCP UDP port.
func (e *pionEndpoint) LocalPort() int { return e.port }

// Close tears down the PeerConnection and the UDP mux, unblocking any pending ReadRTP.
// Idempotent.
func (e *pionEndpoint) Close() error {
	var err error
	e.closeOnce.Do(func() {
		e.readyOnce.Do(func() { close(e.ready) })
		err = e.pc.Close()
		_ = e.mux.Close()
		_ = e.udpConn.Close()
	})
	return err
}
