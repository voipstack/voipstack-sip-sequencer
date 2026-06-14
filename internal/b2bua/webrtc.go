package b2bua

import (
	"fmt"
	"net"
	"sync"

	"github.com/pion/ice/v4"
	"github.com/pion/logging"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
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
	// relay consumes).
	ReadRTP(buf []byte) (int, error)
	// WriteRTP sends one plaintext RTP packet toward the webphone, encrypting it to
	// SRTP. A write before the leg is up is dropped, not an error.
	WriteRTP(pkt []byte) (int, error)
	// ReadRTCP yields one decrypted RTCP packet from the endpoint (rtcp-mux'd port).
	ReadRTCP(buf []byte) (int, error)
	// WriteRTCP sends one plaintext RTCP packet toward the webphone over the muxed port.
	WriteRTCP(pkt []byte) (int, error)
	// LocalPort is the UDP port the endpoint listens on (RTP and RTCP share it).
	LocalPort() int
	// AddRemoteCandidate feeds one trickled ICE candidate (the candidate:… attribute
	// value, no a= prefix) into the endpoint's connectivity checks.
	AddRemoteCandidate(candidate string) error
	// EndOfRemoteCandidates signals that the remote candidate list is complete.
	EndOfRemoteCandidates() error
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

// WriteRTP encrypts and sends one plaintext RTP packet toward the webphone.
func (l *SecuredLeg) WriteRTP(pkt []byte) (int, error) { return l.endpoint.WriteRTP(pkt) }

// ReadRTCP yields one decrypted RTCP packet from the endpoint.
func (l *SecuredLeg) ReadRTCP(buf []byte) (int, error) { return l.endpoint.ReadRTCP(buf) }

// WriteRTCP encrypts and sends one plaintext RTCP packet toward the webphone.
func (l *SecuredLeg) WriteRTCP(pkt []byte) (int, error) { return l.endpoint.WriteRTCP(pkt) }

// AnswerSDP returns the ICE-lite/DTLS-SRTP SDP answer to send back to the webphone.
func (l *SecuredLeg) AnswerSDP() []byte { return l.answerSDP }

// AddRemoteCandidate delegates a trickled ICE candidate to the underlying endpoint.
func (l *SecuredLeg) AddRemoteCandidate(c string) error { return l.endpoint.AddRemoteCandidate(c) }

// EndOfRemoteCandidates delegates end-of-candidates to the underlying endpoint.
func (l *SecuredLeg) EndOfRemoteCandidates() error { return l.endpoint.EndOfRemoteCandidates() }

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

	ready      chan struct{} // closed when the remote track arrives or on Close
	readyOnce  sync.Once     // guards close(ready)
	mu         sync.Mutex    // guards track, receiver, localTrack
	track      *webrtc.TrackRemote
	receiver   *webrtc.RTPReceiver         // inbound RTCP source (rtcp-mux'd port)
	localTrack *webrtc.TrackLocalStaticRTP // outbound: pion encrypts writes to SRTP
	closeOnce  sync.Once
}

// Answer drives ICE-lite gathering and the DTLS-SRTP setup, returning the SDP answer.
// rtcp-mux and the DTLS fingerprint are produced by pion in the answer. The OnTrack
// handler captures the inbound plaintext RTP track (for ReadRTP) and its receiver (for
// ReadRTCP). A local sendable track is added so the answer is sendrecv and pion
// encrypts outbound RTP to SRTP for WriteRTP.
func (e *pionEndpoint) Answer(offer []byte) ([]byte, error) {
	e.pc.OnTrack(func(tr *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
		e.mu.Lock()
		e.track = tr
		e.receiver = recv
		e.mu.Unlock()
		e.readyOnce.Do(func() { close(e.ready) })
	})

	if err := e.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  string(offer),
	}); err != nil {
		return nil, fmt.Errorf("set remote description: %w", err)
	}

	// Add a local outbound track so the answer advertises sendrecv and pion encrypts
	// plaintext RTP written via WriteRTP into SRTP toward the webphone. Opus is the
	// browser webphone's codec; the bridge's byte-for-byte forwarding is codec-agnostic
	// and proven independently of this concrete track.
	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus}, "audio", "anchor")
	if err != nil {
		return nil, fmt.Errorf("create local track: %w", err)
	}
	if _, err := e.pc.AddTrack(localTrack); err != nil {
		return nil, fmt.Errorf("add local track: %w", err)
	}
	e.mu.Lock()
	e.localTrack = localTrack
	e.mu.Unlock()

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

// WriteRTP encrypts and sends one plaintext RTP packet toward the webphone via the
// local track. pion rewrites the SSRC/payload type to the negotiated values and
// encrypts to SRTP. A write before the local track exists (leg not yet answered) is
// dropped, not an error; pion itself drops writes before a peer binds.
func (e *pionEndpoint) WriteRTP(pkt []byte) (int, error) {
	e.mu.Lock()
	tr := e.localTrack
	e.mu.Unlock()
	if tr == nil {
		return len(pkt), nil
	}
	var p rtp.Packet
	if err := p.Unmarshal(pkt); err != nil {
		return 0, fmt.Errorf("unmarshal rtp: %w", err)
	}
	if err := tr.WriteRTP(&p); err != nil {
		return 0, err
	}
	return len(pkt), nil
}

// ReadRTCP blocks until the inbound track (and its receiver) is available, then reads
// one decrypted RTCP packet from the rtcp-mux'd port. A Close before the receiver
// arrives unblocks and returns an error rather than hanging.
func (e *pionEndpoint) ReadRTCP(buf []byte) (int, error) {
	<-e.ready
	e.mu.Lock()
	recv := e.receiver
	e.mu.Unlock()
	if recv == nil {
		return 0, fmt.Errorf("webrtc endpoint closed before receiver ready")
	}
	n, _, err := recv.Read(buf)
	return n, err
}

// WriteRTCP sends one plaintext RTCP packet toward the webphone over the muxed port.
// The bytes are parsed into RTCP packets and handed to the PeerConnection, which
// encrypts them. Best-effort: an unparseable RTCP packet is dropped.
func (e *pionEndpoint) WriteRTCP(pkt []byte) (int, error) {
	pkts, err := rtcp.Unmarshal(pkt)
	if err != nil {
		return 0, fmt.Errorf("unmarshal rtcp: %w", err)
	}
	if err := e.pc.WriteRTCP(pkts); err != nil {
		return 0, err
	}
	return len(pkt), nil
}

// LocalPort returns the shared RTP/RTCP UDP port.
func (e *pionEndpoint) LocalPort() int { return e.port }

// AddRemoteCandidate adds one trickled remote ICE candidate to the agent. pion accepts
// remote candidates after SetRemoteDescription; the candidate string is the
// candidate:… attribute value with no a= prefix.
func (e *pionEndpoint) AddRemoteCandidate(candidate string) error {
	idx := uint16(0)
	if err := e.pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidate, SDPMLineIndex: &idx}); err != nil {
		return fmt.Errorf("add ice candidate: %w", err)
	}
	return nil
}

// EndOfRemoteCandidates marks the remote candidate list complete. pion's convention is
// an empty-candidate AddICECandidate.
func (e *pionEndpoint) EndOfRemoteCandidates() error {
	if err := e.pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: ""}); err != nil {
		return fmt.Errorf("end of remote candidates: %w", err)
	}
	return nil
}

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
