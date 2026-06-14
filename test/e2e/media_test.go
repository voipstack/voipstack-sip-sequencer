//go:build e2e

package e2e

import (
	"bytes"
	"net"
	"strconv"
	"testing"
	"time"
)

// anchor is one side's anchored RTP host/port as advertised by the engine.
type anchor struct {
	host string
	port int
}

func (a anchor) udp(portOffset int) *net.UDPAddr {
	return &net.UDPAddr{IP: net.ParseIP(a.host), Port: a.port + portOffset}
}

// anchoredCall drives a full UDP INVITE/ACK offering epOffer, has the PBX answer
// with pbxAnswer, and returns the engine's endpoint-facing and PBX-facing RTP
// anchors (the endpoint anchor from the caller's 200, the PBX anchor from the offer
// the engine sent the PBX). The caller ACKs, so media is permitted to flow.
func anchoredCall(t *testing.T, caller *fakeUAC, sipListen string, pbx *fakeUAS, epOffer, pbxAnswer []byte) (epA, pbxA anchor) {
	t.Helper()
	return anchoredCallTo(t, caller, "sip:"+sipListen, pbx, epOffer, pbxAnswer)
}

// anchoredCallTo is anchoredCall against an explicit target URI (e.g. a TLS address
// with a transport param), so a secure inbound caller can be exercised.
func anchoredCallTo(t *testing.T, caller *fakeUAC, targetURI string, pbx *fakeUAS, epOffer, pbxAnswer []byte) (epA, pbxA anchor) {
	t.Helper()
	var pbxOffer []byte
	done := make(chan struct{})
	go func() {
		dss := pbx.waitInvite(t, 5*time.Second)
		pbxOffer = dss.InviteRequest.Body()
		_ = dss.Respond(200, "OK", pbxAnswer)
		close(done)
	}()

	sess := establishWithOffer(t, caller, targetURI, epOffer)
	<-done

	eh, ep := parseAudioConn(t, sess.InviteResponse.Body())
	ph, pp := parseAudioConn(t, pbxOffer)
	return anchor{eh, ep}, anchor{ph, pp}
}

// Given an established call; When RTP is sent toward each anchored port; Then the
// far side receives it FROM the sequencer's facing port (not the original sender's)
// — proving the B2BUA re-originates media, in both directions.
func TestRTPRelayedFromAnchoredSourcePorts(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	epConn, epHost, epPort := udpSocket(t)
	pbxConn, pbxHost, pbxPort := udpSocket(t)

	serveApp(t, appSrv, "A", nil, 200, "OK", []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "none", "abort")})
	s := startReady(t, cfg)

	epA, pbxA := anchoredCall(t, caller, s.sipListen, pbx,
		sdpWithAddr(epHost, epPort), sdpWithAddr(pbxHost, pbxPort))

	// endpoint → PBX arrives from the PBX-facing anchor port.
	up := []byte{0x80, 0x00, 0xCA, 0xFE}
	sendUDP(t, epConn, epA.host, epA.port, up)
	got, from := recvUDPFrom(t, pbxConn, 2*time.Second)
	if !bytes.Equal(got, up) {
		t.Fatalf("endpoint→PBX payload: got %v, want %v", got, up)
	}
	if from.Port != pbxA.port {
		t.Fatalf("endpoint→PBX arrived from port %d, want PBX-facing %d", from.Port, pbxA.port)
	}

	// PBX → endpoint arrives from the endpoint-facing anchor port.
	down := []byte{0x80, 0x00, 0xBE, 0xEF}
	sendUDP(t, pbxConn, pbxA.host, pbxA.port, down)
	got, from = recvUDPFrom(t, epConn, 2*time.Second)
	if !bytes.Equal(got, down) {
		t.Fatalf("PBX→endpoint payload: got %v, want %v", got, down)
	}
	if from.Port != epA.port {
		t.Fatalf("PBX→endpoint arrived from port %d, want endpoint-facing %d", from.Port, epA.port)
	}
}

// Given an established call; When a burst of distinct RTP packets is sent each way;
// Then every packet is relayed intact and in order — confirming sustained,
// bidirectional media transmission, not just a single primed packet.
func TestSustainedBidirectionalRTP(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	epConn, epHost, epPort := udpSocket(t)
	pbxConn, pbxHost, pbxPort := udpSocket(t)

	serveApp(t, appSrv, "A", nil, 200, "OK", []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "none", "abort")})
	s := startReady(t, cfg)

	epA, pbxA := anchoredCall(t, caller, s.sipListen, pbx,
		sdpWithAddr(epHost, epPort), sdpWithAddr(pbxHost, pbxPort))

	const n = 16
	// endpoint → PBX
	for i := 0; i < n; i++ {
		pkt := rtpPacket(i)
		sendUDP(t, epConn, epA.host, epA.port, pkt)
		if got, _ := recvUDPFrom(t, pbxConn, 2*time.Second); !bytes.Equal(got, pkt) {
			t.Fatalf("endpoint→PBX packet %d: got %v, want %v", i, got, pkt)
		}
	}
	// PBX → endpoint
	for i := 0; i < n; i++ {
		pkt := rtpPacket(100 + i)
		sendUDP(t, pbxConn, pbxA.host, pbxA.port, pkt)
		if got, _ := recvUDPFrom(t, epConn, 2*time.Second); !bytes.Equal(got, pkt) {
			t.Fatalf("PBX→endpoint packet %d: got %v, want %v", i, got, pkt)
		}
	}
}

// Given a call routed through a two-application chain; When RTP flows; Then media is
// still relayed endpoint↔PBX — the application legs do not disturb the call's anchor.
func TestMediaFlowsWithAppChainPresent(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	epConn, epHost, epPort := udpSocket(t)
	pbxConn, pbxHost, pbxPort := udpSocket(t)

	serveApp(t, appA, "A", nil, 200, "OK", []byte(testSDP2))
	serveApp(t, appB, "B", nil, 200, "OK", []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{
		app("A", appA, "none", "skip"),
		app("B", appB, "none", "skip"),
	})
	s := startReady(t, cfg)

	epA, pbxA := anchoredCall(t, caller, s.sipListen, pbx,
		sdpWithAddr(epHost, epPort), sdpWithAddr(pbxHost, pbxPort))

	up := []byte{0x80, 0x00, 0x11, 0x22}
	sendUDP(t, epConn, epA.host, epA.port, up)
	if got, _ := recvUDPFrom(t, pbxConn, 2*time.Second); !bytes.Equal(got, up) {
		t.Fatalf("endpoint→PBX with app chain: got %v, want %v", got, up)
	}

	down := []byte{0x80, 0x00, 0x33, 0x44}
	sendUDP(t, pbxConn, pbxA.host, pbxA.port, down)
	if got, _ := recvUDPFrom(t, epConn, 2*time.Second); !bytes.Equal(got, down) {
		t.Fatalf("PBX→endpoint with app chain: got %v, want %v", got, down)
	}
}

// Given an established call; When RTCP is sent to the anchored RTCP port (RTP+1);
// Then it is relayed to the far side's RTCP port — confirming the control channel
// transmits, not only RTP.
func TestRTCPRelayedThroughSequencer(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	epRTP, epRTCP, epHost, epPort := rtpPair(t)
	pbxRTP, pbxRTCP, pbxHost, pbxPort := rtpPair(t)
	_ = epRTP
	_ = pbxRTP

	serveApp(t, appSrv, "A", nil, 200, "OK", []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "none", "abort")})
	s := startReady(t, cfg)

	epA, pbxA := anchoredCall(t, caller, s.sipListen, pbx,
		sdpWithAddr(epHost, epPort), sdpWithAddr(pbxHost, pbxPort))

	// A minimal RTCP Sender Report. RTCP rides RTP port + 1 by convention.
	sr := []byte{0x80, 0xC8, 0x00, 0x06}
	if _, err := epRTCP.WriteToUDP(sr, epA.udp(1)); err != nil {
		t.Fatalf("send RTCP endpoint→seq: %v", err)
	}
	if got, _ := recvUDPFrom(t, pbxRTCP, 2*time.Second); !bytes.Equal(got, sr) {
		t.Fatalf("endpoint→PBX RTCP not relayed: got %v, want %v", got, sr)
	}

	if _, err := pbxRTCP.WriteToUDP(sr, pbxA.udp(1)); err != nil {
		t.Fatalf("send RTCP PBX→seq: %v", err)
	}
	if got, _ := recvUDPFrom(t, epRTCP, 2*time.Second); !bytes.Equal(got, sr) {
		t.Fatalf("PBX→endpoint RTCP not relayed: got %v, want %v", got, sr)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// rtpPacket builds a tiny distinct RTP-shaped packet for sequence i.
func rtpPacket(i int) []byte {
	return []byte{0x80, 0x00, byte(i >> 8), byte(i), 0xAA, byte(i)}
}

// recvUDPFrom reads one packet and returns its payload and source address.
func recvUDPFrom(t *testing.T, conn *net.UDPConn, timeout time.Duration) ([]byte, *net.UDPAddr) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 1500)
	n, addr, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("recv UDP: %v", err)
	}
	return buf[:n], addr
}

// rtpPair opens an RTP socket on an even-ish free port and an RTCP socket on the
// next port, retrying until a consecutive pair is free.
func rtpPair(t *testing.T) (rtp, rtcp *net.UDPConn, host string, port int) {
	t.Helper()
	for attempts := 0; attempts < 50; attempts++ {
		r, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatalf("rtpPair rtp listen: %v", err)
		}
		_, ps, _ := net.SplitHostPort(r.LocalAddr().String())
		p := mustAtoiT(t, ps)
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: p + 1})
		if err != nil {
			r.Close() // p+1 taken; try again
			continue
		}
		t.Cleanup(func() { r.Close(); c.Close() })
		return r, c, "127.0.0.1", p
	}
	t.Fatal("rtpPair: could not find a consecutive RTP/RTCP port pair")
	return nil, nil, "", 0
}

func mustAtoiT(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("atoi %q: %v", s, err)
	}
	return n
}
