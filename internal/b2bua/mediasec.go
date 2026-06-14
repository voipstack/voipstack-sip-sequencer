package b2bua

// MediaSecurity is the security profile of one media leg. It is a per-leg property:
// a leg is plain RTP or DTLS-SRTP independently of the other leg, so an SRTP↔SRTP
// proxy can later be enabled by setting the opposite leg's property without reworking
// this one. Nothing assumes "webphone = SRTP, other = RTP" as a fixed rule.
type MediaSecurity int

const (
	// SecurityPlainRTP is an unencrypted RTP/RTCP leg (the existing anchor side).
	SecurityPlainRTP MediaSecurity = iota
	// SecurityDTLSSRTP is a DTLS-SRTP (WebRTC) leg whose keys are derived from a
	// terminated DTLS handshake.
	SecurityDTLSSRTP
)

func (s MediaSecurity) String() string {
	switch s {
	case SecurityPlainRTP:
		return "plain-rtp"
	case SecurityDTLSSRTP:
		return "dtls-srtp"
	default:
		return "unknown"
	}
}

// MediaLeg is one anchored media endpoint, abstracted over its security profile. Both
// the plain AnchorSide and the secured WebRTC leg satisfy it, so the media relay can
// treat them uniformly via plaintext RTP/RTCP. Read* yields decrypted plaintext and
// Write* accepts plaintext, each applying the leg's own on-the-wire security. Because
// encrypt/decrypt is a property of the leg and not of the relay, an SRTP↔SRTP proxy is
// later enabled by changing a leg's security alone — the forwarding path is unchanged.
type MediaLeg interface {
	Security() MediaSecurity
	// ReadRTP yields one decrypted/plaintext RTP packet from the leg.
	ReadRTP(buf []byte) (int, error)
	// WriteRTP sends one plaintext RTP packet on the leg, applying the leg's outbound
	// security (plain = raw write; secured = encrypt to SRTP).
	WriteRTP(pkt []byte) (int, error)
	// ReadRTCP yields one decrypted/plaintext RTCP packet from the leg.
	ReadRTCP(buf []byte) (int, error)
	// WriteRTCP sends one plaintext RTCP packet on the leg, applying outbound security.
	WriteRTCP(pkt []byte) (int, error)
	Close()
}
