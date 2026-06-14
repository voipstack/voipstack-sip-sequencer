package b2bua

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// ── bridgeLegs behavior (STORY-001-021) ──────────────────────────────────────
//
// These tests exercise the security-agnostic media bridge directly: a secured leg
// (the WebRTC fake, yielding canned *decrypted* RTP/RTCP and recording the plaintext
// written to it — the encrypt boundary) bridged to a plain RTP AnchorSide on real UDP
// sockets. The WebRTC peer is the only external boundary that is faked (AGENTS.md);
// AnchorSide is tested against real sockets.

// bindAnchorSide binds a plain RTP/RTCP AnchorSide on ephemeral 127.0.0.1 ports.
func bindAnchorSide(t *testing.T) *AnchorSide {
	t.Helper()
	rtpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("bind rtp: %v", err)
	}
	rtcpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		rtpConn.Close()
		t.Fatalf("bind rtcp: %v", err)
	}
	return &AnchorSide{
		rtpConn:      rtpConn,
		rtcpConn:     rtcpConn,
		localRTPPort: rtpConn.LocalAddr().(*net.UDPAddr).Port,
	}
}

// udpPeer is a UDP socket standing in for the opposite (plain-RTP) party.
func udpPeer(t *testing.T) (*net.UDPConn, *net.UDPAddr) {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("udp peer: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c, c.LocalAddr().(*net.UDPAddr)
}

func readUDP(t *testing.T, c *net.UDPConn, d time.Duration) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(d))
	buf := make([]byte, 1500)
	n, _, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read udp: %v", err)
	}
	return append([]byte(nil), buf[:n]...)
}

func dialUDP(t *testing.T, c *net.UDPConn) *net.UDPConn {
	t.Helper()
	conn, err := net.DialUDP("udp", nil, c.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("dial udp: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func waitFor(t *testing.T, cond func() bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// AC1/AC3/AC5: RTP arriving (decrypted) from the secured webphone leg is forwarded as
// plain RTP to the opposite leg, byte-for-byte (no transcoding).
func TestBridgeForwardsDecryptedRTPToPlainLeg(t *testing.T) {
	fake := newBridgeFakeEndpoint()
	secured := &SecuredLeg{endpoint: fake}
	plain := bindAnchorSide(t)

	peer, peerAddr := udpPeer(t)
	plain.setRemote(peerAddr, nil)

	m := &MediaSession{endpointLeg: secured, pbxSide: plain}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.bridgeLegs(ctx, secured, plain, nil, nil)

	payload := []byte{0x80, 0x6f, 0x12, 0x34, 0xde, 0xad, 0xbe, 0xef}
	fake.inRTP <- payload

	got := readUDP(t, peer, 2*time.Second)
	if string(got) != string(payload) {
		t.Fatalf("forwarded RTP = %v, want byte-identical %v", got, payload)
	}
}

// AC1/AC3: plain RTP from the opposite leg is handed as plaintext to the secured leg,
// which encrypts it toward the webphone — the security boundary is at the anchor.
func TestBridgeEncryptsPlainRTPToSecuredLeg(t *testing.T) {
	fake := newBridgeFakeEndpoint()
	secured := &SecuredLeg{endpoint: fake}
	plain := bindAnchorSide(t)

	m := &MediaSession{endpointLeg: secured, pbxSide: plain}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.bridgeLegs(ctx, secured, plain, nil, nil)

	sender := dialUDP(t, plain.rtpConn)
	payload := []byte{0x80, 0x00, 0xca, 0xfe, 0x01, 0x02}
	if _, err := sender.Write(payload); err != nil {
		t.Fatalf("send plain RTP: %v", err)
	}

	waitFor(t, func() bool { return len(fake.writtenRTP()) > 0 }, 2*time.Second)
	got := fake.writtenRTP()[0]
	if string(got) != string(payload) {
		t.Fatalf("plaintext into secured leg = %v, want byte-identical %v", got, payload)
	}
}

// AC4: RTCP is bridged alongside RTP in both directions.
func TestBridgeRTCPBothDirections(t *testing.T) {
	fake := newBridgeFakeEndpoint()
	secured := &SecuredLeg{endpoint: fake}
	plain := bindAnchorSide(t)

	rtcpPeer, rtcpPeerAddr := udpPeer(t)
	plain.setRemote(nil, rtcpPeerAddr)

	m := &MediaSession{endpointLeg: secured, pbxSide: plain}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.bridgeLegs(ctx, secured, plain, nil, nil)

	// webphone → opposite leg (decrypted RTCP forwarded plain).
	sr := []byte{0x80, 0xc8, 0x00, 0x06, 0xaa, 0xbb, 0xcc, 0xdd}
	fake.inRTCP <- sr
	got := readUDP(t, rtcpPeer, 2*time.Second)
	if string(got) != string(sr) {
		t.Fatalf("forwarded RTCP = %v, want %v", got, sr)
	}

	// opposite leg → webphone (plain RTCP handed to secured leg for encryption).
	sender := dialUDP(t, plain.rtcpConn)
	rr := []byte{0x81, 0xc9, 0x00, 0x07, 0x11, 0x22, 0x33, 0x44}
	if _, err := sender.Write(rr); err != nil {
		t.Fatalf("send RTCP: %v", err)
	}
	waitFor(t, func() bool { return len(fake.writtenRTCP()) > 0 }, 2*time.Second)
	if got := fake.writtenRTCP()[0]; string(got) != string(rr) {
		t.Fatalf("RTCP into secured leg = %v, want %v", got, rr)
	}
}

// NFR (per-leg security independence): bridgeLegs references only MediaLeg, so it
// forwards between two plain legs unchanged — the same forwarding path that bridges a
// secured leg. Enabling SRTP on a leg later needs no change here.
func TestBridgeForwardsBetweenTwoPlainLegs(t *testing.T) {
	a := bindAnchorSide(t)
	b := bindAnchorSide(t)

	peer, peerAddr := udpPeer(t)
	b.setRemote(peerAddr, nil)

	m := &MediaSession{endpointSide: a, pbxSide: b}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.bridgeLegs(ctx, a, b, nil, nil)

	sender := dialUDP(t, a.rtpConn)
	payload := []byte{0x80, 0x08, 0xbe, 0xef}
	if _, err := sender.Write(payload); err != nil {
		t.Fatalf("send RTP: %v", err)
	}
	got := readUDP(t, peer, 2*time.Second)
	if string(got) != string(payload) {
		t.Fatalf("plain↔plain forward = %v, want %v", got, payload)
	}
}

// AC6: a media-plane failure on one direction (e.g. SRTP write/auth failure) is
// best-effort and confined — it stops only that direction; the opposite direction keeps
// flowing.
func TestBridgeMediaFailureStaysIsolated(t *testing.T) {
	fake := newBridgeFakeEndpoint()
	fake.writeErr = errors.New("srtp auth failure")
	secured := &SecuredLeg{endpoint: fake}
	plain := bindAnchorSide(t)

	peer, peerAddr := udpPeer(t)
	plain.setRemote(peerAddr, nil)

	m := &MediaSession{endpointLeg: secured, pbxSide: plain}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.bridgeLegs(ctx, secured, plain, nil, nil)

	// Drive the failing direction: opposite → webphone write errors (encrypt failure).
	sender := dialUDP(t, plain.rtpConn)
	if _, err := sender.Write([]byte{0x80, 0x00, 0x01, 0x02}); err != nil {
		t.Fatalf("send into failing direction: %v", err)
	}

	// The surviving direction (webphone → opposite) still forwards.
	payload := []byte{0x80, 0x6f, 0x55, 0x66}
	fake.inRTP <- payload
	got := readUDP(t, peer, 2*time.Second)
	if string(got) != string(payload) {
		t.Fatalf("surviving direction forwarded %v, want %v", got, payload)
	}
}
