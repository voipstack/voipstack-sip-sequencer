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
// treat them uniformly via plaintext RTP. ReadRTP yields decrypted RTP regardless of
// the leg's on-the-wire security.
type MediaLeg interface {
	Security() MediaSecurity
	ReadRTP(buf []byte) (int, error)
	Close()
}
