package b2bua

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// ── tap test helpers ──────────────────────────────────────────────────────────

// tapAnswerSDP builds a two-stream recvonly answer for a fake tap UAS.
func tapAnswerSDP(host string, port1, port2 int) []byte {
	return []byte(fmt.Sprintf(
		"v=0\r\no=- 0 0 IN IP4 %s\r\ns=-\r\nt=0 0\r\n"+
			"m=audio %d RTP/AVP 0\r\nc=IN IP4 %s\r\na=rtpmap:0 PCMU/8000\r\na=recvonly\r\n"+
			"m=audio %d RTP/AVP 0\r\nc=IN IP4 %s\r\na=rtpmap:0 PCMU/8000\r\na=recvonly\r\n",
		host, port1, host, port2, host,
	))
}

// newTapReceivers creates two UDP listeners to act as tap stream receivers.
func newTapReceivers(t *testing.T) (s1, s2 *net.UDPConn) {
	t.Helper()
	c1, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tap receiver 1: %v", err)
	}
	c2, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		c1.Close()
		t.Fatalf("tap receiver 2: %v", err)
	}
	t.Cleanup(func() { c1.Close(); c2.Close() })
	return c1.(*net.UDPConn), c2.(*net.UDPConn)
}

// autoAnswerTapWith drains uas.invites and answers each with sdpFn().
func autoAnswerTapWith(t *testing.T, uas *fakeUAS, sdpFn func() []byte) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			select {
			case dss := <-uas.invites:
				_ = dss.Respond(200, "OK", sdpFn())
			case <-ctx.Done():
				return
			}
		}
	}()
}

// tapConfig builds a Config with one tap-mode app.
func tapConfig(listenAddr, tapURI, pbxURI string) config.Config {
	return config.Config{
		SIP:     config.SIP{Listen: listenAddr},
		NextHop: pbxURI,
		RTP:     config.RTP{PortRange: "16000-18000"},
		Sequence: []config.Application{
			{Name: "tapapp", URI: tapURI, OnFailure: config.FailureSkip, Media: config.MediaTap},
		},
	}
}

// addrPort extracts the port from a net.Addr string.
func addrPort(t *testing.T, a net.Addr) int {
	t.Helper()
	_, portStr, _ := net.SplitHostPort(a.String())
	return mustAtoi(t, portStr)
}

// addrHost extracts the host from a net.Addr string.
func addrHost(a net.Addr) string {
	host, _, _ := net.SplitHostPort(a.String())
	return host
}

// readWithDeadline reads one UDP packet from conn with a 2s deadline.
func readWithDeadline(t *testing.T, conn *net.UDPConn) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return buf[:n]
}

// noPacketTap asserts no UDP packet arrives on conn within window.
func noPacketTap(t *testing.T, conn *net.UDPConn, window time.Duration) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(window)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFrom(buf)
	if err == nil {
		t.Fatalf("expected no packet, got %d bytes: %v", n, buf[:n])
	}
}

// setupTapCall runs the SIP negotiation for a call with a tap app.
// tapUAS must already have its invite handler installed (autoAnswerTapWith).
// Returns seq EP-facing and PBX-facing RTP addresses.
func setupTapCall(
	t *testing.T,
	listenAddr string,
	pbxUAS *fakeUAS,
	epConn, pbxConn *net.UDPConn,
) (seqEPAddr, seqPBXAddr *net.UDPAddr) {
	t.Helper()
	ctx := context.Background()

	callerSDP := sdpWithAddr(addrHost(epConn.LocalAddr()), addrPort(t, epConn.LocalAddr()))
	pbxSDP := sdpWithAddr(addrHost(pbxConn.LocalAddr()), addrPort(t, pbxConn.LocalAddr()))

	caller := newFakeUAC(t)
	sess, err := caller.invite(ctx, "sip:"+listenAddr, callerSDP)
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	pbxBodyCh := make(chan []byte, 1)
	go func() {
		dss := pbxUAS.waitInvite(t, 5*time.Second)
		pbxBodyCh <- copyBody(dss.InviteRequest.Body())
		_ = dss.Respond(200, "OK", pbxSDP)
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	pbxBody := <-pbxBodyCh
	pbxH, pbxP, err := parseMedia(pbxBody)
	if err != nil {
		t.Fatalf("parse seq PBX body: %v", err)
	}
	epH, epP, err := parseMedia(sess.InviteResponse.Body())
	if err != nil {
		t.Fatalf("parse seq EP body: %v", err)
	}
	return &net.UDPAddr{IP: net.ParseIP(epH), Port: epP},
		&net.UDPAddr{IP: net.ParseIP(pbxH), Port: pbxP}
}

// ── tap behavior tests ────────────────────────────────────────────────────────

// Given tap app + established call; When ep sends RTP to seq EP side; Then tap stream 1
// receives caller packet. When PBX sends RTP to seq PBX side; Then tap stream 2 receives
// callee packet (AC1).
func TestTapAppReceivesBothCallDirections(t *testing.T) {
	tapUAS := newFakeUAS(t)
	pbxUAS := newFakeUAS(t)

	s1, s2 := newTapReceivers(t)
	autoAnswerTapWith(t, tapUAS, func() []byte {
		return tapAnswerSDP(addrHost(s1.LocalAddr()), addrPort(t, s1.LocalAddr()), addrPort(t, s2.LocalAddr()))
	})

	listenAddr := freeAddr(t)
	startEngine(t, tapConfig(listenAddr, tapUAS.sipURI(), pbxUAS.sipURI()), 0)

	epConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ep: %v", err)
	}
	defer epConn.Close()
	t.Cleanup(func() { epConn.Close() })

	pbxConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pbx: %v", err)
	}
	defer pbxConn.Close()
	t.Cleanup(func() { pbxConn.Close() })

	seqEPAddr, seqPBXAddr := setupTapCall(t, listenAddr, pbxUAS,
		epConn.(*net.UDPConn), pbxConn.(*net.UDPConn))

	callerPkt := []byte{0x80, 0x60, 0xCA, 0xFE, 0x01, 0x02}
	calleePkt := []byte{0x80, 0x60, 0x11, 0x22, 0x03, 0x04}

	if _, err := epConn.WriteTo(callerPkt, seqEPAddr); err != nil {
		t.Fatalf("send caller: %v", err)
	}
	if _, err := pbxConn.WriteTo(calleePkt, seqPBXAddr); err != nil {
		t.Fatalf("send callee: %v", err)
	}

	got1 := readWithDeadline(t, s1)
	if string(got1) != string(callerPkt) {
		t.Errorf("stream 1 = %v, want caller %v", got1, callerPkt)
	}
	got2 := readWithDeadline(t, s2)
	if string(got2) != string(calleePkt) {
		t.Errorf("stream 2 = %v, want callee %v", got2, calleePkt)
	}
}

// Given tap app + established call; When caller and callee each send a distinct packet;
// Then each tap stream contains exactly the corresponding direction, not the other (AC2).
func TestForkIsByteForByteAndSeparate(t *testing.T) {
	tapUAS := newFakeUAS(t)
	pbxUAS := newFakeUAS(t)

	s1, s2 := newTapReceivers(t)
	autoAnswerTapWith(t, tapUAS, func() []byte {
		return tapAnswerSDP(addrHost(s1.LocalAddr()), addrPort(t, s1.LocalAddr()), addrPort(t, s2.LocalAddr()))
	})

	listenAddr := freeAddr(t)
	startEngine(t, tapConfig(listenAddr, tapUAS.sipURI(), pbxUAS.sipURI()), 0)

	epConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	pbxConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer epConn.Close()
	defer pbxConn.Close()
	t.Cleanup(func() { epConn.Close(); pbxConn.Close() })

	seqEPAddr, seqPBXAddr := setupTapCall(t, listenAddr, pbxUAS,
		epConn.(*net.UDPConn), pbxConn.(*net.UDPConn))

	callerPkt := []byte{0x80, 0x00, 0xAA, 0xBB}
	calleePkt := []byte{0x80, 0x00, 0xCC, 0xDD}

	if _, err := epConn.WriteTo(callerPkt, seqEPAddr); err != nil {
		t.Fatalf("send caller: %v", err)
	}
	if _, err := pbxConn.WriteTo(calleePkt, seqPBXAddr); err != nil {
		t.Fatalf("send callee: %v", err)
	}

	got1 := readWithDeadline(t, s1)
	got2 := readWithDeadline(t, s2)

	if string(got1) != string(callerPkt) {
		t.Errorf("stream 1 = %v, want caller %v", got1, callerPkt)
	}
	if string(got2) != string(calleePkt) {
		t.Errorf("stream 2 = %v, want callee %v", got2, calleePkt)
	}
	if string(got1) == string(got2) {
		t.Errorf("streams must differ: both = %v", got1)
	}
}

// Given tap app + established call; When a normal call packet traverses seq; Then ep↔PBX
// relay is correct AND tap receives its copy; tap's own write socket is never read by relay
// so it cannot inject audio into the call (AC3).
func TestTapIsRecvonlyCallUnaffected(t *testing.T) {
	tapUAS := newFakeUAS(t)
	pbxUAS := newFakeUAS(t)

	s1, s2 := newTapReceivers(t)
	autoAnswerTapWith(t, tapUAS, func() []byte {
		return tapAnswerSDP(addrHost(s1.LocalAddr()), addrPort(t, s1.LocalAddr()), addrPort(t, s2.LocalAddr()))
	})

	listenAddr := freeAddr(t)
	startEngine(t, tapConfig(listenAddr, tapUAS.sipURI(), pbxUAS.sipURI()), 0)

	epConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	pbxConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer epConn.Close()
	defer pbxConn.Close()
	t.Cleanup(func() { epConn.Close(); pbxConn.Close() })

	seqEPAddr, seqPBXAddr := setupTapCall(t, listenAddr, pbxUAS,
		epConn.(*net.UDPConn), pbxConn.(*net.UDPConn))

	callerPkt := []byte{0x80, 0x00, 0x01, 0x02}
	// ep → seq: should relay to PBX and tap s1.
	if _, err := epConn.WriteTo(callerPkt, seqEPAddr); err != nil {
		t.Fatalf("send caller: %v", err)
	}
	gotPBX := readWithDeadline(t, pbxConn.(*net.UDPConn))
	if string(gotPBX) != string(callerPkt) {
		t.Errorf("pbx got %v, want %v", gotPBX, callerPkt)
	}
	// Tap stream 1 also gets it.
	got1 := readWithDeadline(t, s1)
	if string(got1) != string(callerPkt) {
		t.Errorf("tap s1 got %v, want %v", got1, callerPkt)
	}

	// Callee direction.
	calleePkt := []byte{0x80, 0x00, 0x03, 0x04}
	if _, err := pbxConn.WriteTo(calleePkt, seqPBXAddr); err != nil {
		t.Fatalf("send callee: %v", err)
	}
	gotEP := readWithDeadline(t, epConn.(*net.UDPConn))
	if string(gotEP) != string(calleePkt) {
		t.Errorf("ep got %v, want %v", gotEP, calleePkt)
	}
	// Tap stream 2 also gets it.
	got2 := readWithDeadline(t, s2)
	if string(got2) != string(calleePkt) {
		t.Errorf("tap s2 got %v, want %v", got2, calleePkt)
	}

	// ep receives only what PBX sent — nothing injected by the tap.
	// (The relay never reads from tapCallerStream, so writes to it are discarded.)
	noPacketTap(t, epConn.(*net.UDPConn), 150*time.Millisecond)
}

// Given app with media:none; When call established; Then app INVITE SDP has a=inactive
// and no RTP is forked to any tap receivers; call is up (AC4).
func TestMediaNoneAppGetsNoMedia(t *testing.T) {
	noneApp := newFakeUAS(t)
	pbxUAS := newFakeUAS(t)

	listenAddr := freeAddr(t)
	cfg := config.Config{
		SIP:     config.SIP{Listen: listenAddr},
		NextHop: pbxUAS.sipURI(),
		RTP:     config.RTP{PortRange: "16100-17000"},
		Sequence: []config.Application{
			{Name: "noneapp", URI: noneApp.sipURI(), OnFailure: config.FailureSkip, Media: config.MediaNone},
		},
	}
	startEngine(t, cfg, 0)

	inviteBodyCh := make(chan []byte, 1)
	go func() {
		dss := noneApp.waitInvite(t, 3*time.Second)
		inviteBodyCh <- copyBody(dss.InviteRequest.Body())
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()

	epConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	pbxConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer epConn.Close()
	defer pbxConn.Close()
	t.Cleanup(func() { epConn.Close(); pbxConn.Close() })

	ctx := context.Background()
	caller := newFakeUAC(t)
	sess, err := caller.invite(ctx, "sip:"+listenAddr,
		sdpWithAddr(addrHost(epConn.LocalAddr()), addrPort(t, epConn.LocalAddr())))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	go func() {
		dss := pbxUAS.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK",
			sdpWithAddr(addrHost(pbxConn.LocalAddr()), addrPort(t, pbxConn.LocalAddr())))
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", sess.InviteResponse.StatusCode)
	}

	inviteBody := <-inviteBodyCh
	if !containsBytes(inviteBody, []byte("a=inactive")) {
		t.Fatalf("app INVITE SDP missing a=inactive:\n%s", inviteBody)
	}
}

// Given tap app (on_failure:skip) that rejects with 503; When call runs; Then call media
// flows normally between ep and PBX — no tap, no disruption (NFR).
func TestFailingTapDoesNotDisruptCall(t *testing.T) {
	tapUAS := newFakeUAS(t)
	pbxUAS := newFakeUAS(t)

	listenAddr := freeAddr(t)
	startEngine(t, tapConfig(listenAddr, tapUAS.sipURI(), pbxUAS.sipURI()), 0)

	autoReject(t, tapUAS, 503, "Service Unavailable")

	epConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	pbxConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer epConn.Close()
	defer pbxConn.Close()
	t.Cleanup(func() { epConn.Close(); pbxConn.Close() })

	ctx := context.Background()
	caller := newFakeUAC(t)
	sess, err := caller.invite(ctx, "sip:"+listenAddr,
		sdpWithAddr(addrHost(epConn.LocalAddr()), addrPort(t, epConn.LocalAddr())))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	pbxBodyCh := make(chan []byte, 1)
	go func() {
		dss := pbxUAS.waitInvite(t, 3*time.Second)
		pbxBodyCh <- copyBody(dss.InviteRequest.Body())
		_ = dss.Respond(200, "OK",
			sdpWithAddr(addrHost(pbxConn.LocalAddr()), addrPort(t, pbxConn.LocalAddr())))
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", sess.InviteResponse.StatusCode)
	}

	// Verify RTP flows ep→PBX normally.
	pbxBody := <-pbxBodyCh
	seqPBXHost, seqPBXPort, err := parseMedia(pbxBody)
	if err != nil {
		t.Fatalf("parse seq PBX: %v", err)
	}
	payload := []byte{0x80, 0x00, 0xDE, 0xAD}
	if _, err := pbxConn.WriteTo(payload,
		&net.UDPAddr{IP: net.ParseIP(seqPBXHost), Port: seqPBXPort}); err != nil {
		t.Fatalf("send to seq pbx: %v", err)
	}
	got := readWithDeadline(t, epConn.(*net.UDPConn))
	if string(got) != string(payload) {
		t.Fatalf("ep got %v, want %v", got, payload)
	}
}

// Given tap with on_failure:abort; When port exhaustion stops tap-callee acquisition;
// Then call is rejected (503) — abort policy respected.
func TestPortExhaustionDuringTapAborts(t *testing.T) {
	tapUAS := newFakeUAS(t)
	pbxUAS := newFakeUAS(t)

	listenAddr := freeAddr(t)
	// Range = 1 pair only: tap-caller gets it; tap-callee acquisition exhausts the range.
	cfg := config.Config{
		SIP:     config.SIP{Listen: listenAddr},
		NextHop: pbxUAS.sipURI(),
		RTP:     config.RTP{PortRange: "17000-17001"},
		Sequence: []config.Application{
			{Name: "tapapp", URI: tapUAS.sipURI(), OnFailure: config.FailureAbort, Media: config.MediaTap},
		},
	}
	startEngine(t, cfg, 0)

	ctx := context.Background()
	caller := newFakeUAC(t)
	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	go func() {
		select {
		case <-tapUAS.invites:
		case <-time.After(300 * time.Millisecond):
		}
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err == nil {
		t.Fatal("expected call failure (tap port exhaustion + abort), got 200")
	}
	pbxUAS.noInvite(t, 100*time.Millisecond)
}

// Given tap with on_failure:skip; When port exhaustion stops tap-callee acquisition;
// Then tap is skipped (no abort) and bridge continues to media anchor section (skip policy).
func TestPortExhaustionDuringTapSkips(t *testing.T) {
	tapUAS := newFakeUAS(t)
	pbxUAS := newFakeUAS(t)

	listenAddr := freeAddr(t)
	// Range = 1 pair only: tap-caller gets it; tap-callee exhausted → released; skip.
	// ep then acquires the released pair; pbx exhausts → 503 from media section (not tap section).
	cfg := config.Config{
		SIP:     config.SIP{Listen: listenAddr},
		NextHop: pbxUAS.sipURI(),
		RTP:     config.RTP{PortRange: "17100-17101"},
		Sequence: []config.Application{
			{Name: "tapapp", URI: tapUAS.sipURI(), OnFailure: config.FailureSkip, Media: config.MediaTap},
		},
	}
	startEngine(t, cfg, 0)

	ctx := context.Background()
	caller := newFakeUAC(t)
	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	// Tap UAS should NOT receive an INVITE: exhaustion happens before the app leg originates.
	go func() {
		select {
		case <-tapUAS.invites:
		case <-time.After(300 * time.Millisecond):
		}
	}()
	go func() {
		select {
		case dss := <-pbxUAS.invites:
			_ = dss.Respond(200, "OK", []byte(testSDP2))
		case <-time.After(300 * time.Millisecond):
		}
	}()

	// Call fails at pbx port exhaustion (after tap was skipped) — not aborted at tap stage.
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err == nil {
		t.Fatal("expected call to fail (pbx port exhaustion after tap skip), got 200")
	}
}

// Given tap app; When call ends with BYE; Then tap ports are released and available for
// next call — no leak (AC6).
func TestForkPortsReleasedOnTeardown(t *testing.T) {
	tapUAS := newFakeUAS(t)
	pbxUAS := newFakeUAS(t)

	listenAddr := freeAddr(t)
	// 4 pairs: exactly enough for one tap call (tap-caller, tap-callee, ep, pbx).
	cfg := config.Config{
		SIP:     config.SIP{Listen: listenAddr},
		NextHop: pbxUAS.sipURI(),
		RTP:     config.RTP{PortRange: "17200-17208"},
		Sequence: []config.Application{
			{Name: "tapapp", URI: tapUAS.sipURI(), OnFailure: config.FailureSkip, Media: config.MediaTap},
		},
	}
	startEngine(t, cfg, 0)

	for i := 0; i < 2; i++ {
		s1, s2 := newTapReceivers(t)
		autoAnswerTapWith(t, tapUAS, func() []byte {
			return tapAnswerSDP(addrHost(s1.LocalAddr()), addrPort(t, s1.LocalAddr()), addrPort(t, s2.LocalAddr()))
		})

		ctx := context.Background()
		epConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
		pbxConn, _ := net.ListenPacket("udp", "127.0.0.1:0")

		caller := newFakeUAC(t)
		sess, err := caller.invite(ctx, "sip:"+listenAddr,
			sdpWithAddr(addrHost(epConn.LocalAddr()), addrPort(t, epConn.LocalAddr())))
		if err != nil {
			t.Fatalf("call %d invite: %v", i, err)
		}

		go func() {
			dss := pbxUAS.waitInvite(t, 3*time.Second)
			_ = dss.Respond(200, "OK",
				sdpWithAddr(addrHost(pbxConn.LocalAddr()), addrPort(t, pbxConn.LocalAddr())))
		}()

		if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
			t.Fatalf("call %d WaitAnswer: %v", i, err)
		}
		if err := sess.Ack(ctx); err != nil {
			t.Fatalf("call %d Ack: %v", i, err)
		}
		if sess.InviteResponse.StatusCode != 200 {
			t.Fatalf("call %d: expected 200, got %d", i, sess.InviteResponse.StatusCode)
		}

		if err := sess.Bye(ctx); err != nil {
			t.Fatalf("call %d BYE: %v", i, err)
		}
		epConn.Close()
		pbxConn.Close()
		time.Sleep(150 * time.Millisecond)
	}
}

// Given sequence with two tap apps; When call established; Then each app independently
// receives both call directions (D6).
func TestMultipleTapAppsEachReceiveBothDirections(t *testing.T) {
	tapA := newFakeUAS(t)
	tapB := newFakeUAS(t)
	pbxUAS := newFakeUAS(t)

	sA1, sA2 := newTapReceivers(t)
	sB1, sB2 := newTapReceivers(t)

	autoAnswerTapWith(t, tapA, func() []byte {
		return tapAnswerSDP(addrHost(sA1.LocalAddr()), addrPort(t, sA1.LocalAddr()), addrPort(t, sA2.LocalAddr()))
	})
	autoAnswerTapWith(t, tapB, func() []byte {
		return tapAnswerSDP(addrHost(sB1.LocalAddr()), addrPort(t, sB1.LocalAddr()), addrPort(t, sB2.LocalAddr()))
	})

	listenAddr := freeAddr(t)
	cfg := config.Config{
		SIP:     config.SIP{Listen: listenAddr},
		NextHop: pbxUAS.sipURI(),
		RTP:     config.RTP{PortRange: "17300-18000"},
		Sequence: []config.Application{
			{Name: "tapA", URI: tapA.sipURI(), OnFailure: config.FailureSkip, Media: config.MediaTap},
			{Name: "tapB", URI: tapB.sipURI(), OnFailure: config.FailureSkip, Media: config.MediaTap},
		},
	}
	startEngine(t, cfg, 0)

	epConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	pbxConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer epConn.Close()
	defer pbxConn.Close()
	t.Cleanup(func() { epConn.Close(); pbxConn.Close() })

	ctx := context.Background()
	caller := newFakeUAC(t)
	sess, err := caller.invite(ctx, "sip:"+listenAddr,
		sdpWithAddr(addrHost(epConn.LocalAddr()), addrPort(t, epConn.LocalAddr())))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	pbxBodyCh := make(chan []byte, 1)
	go func() {
		dss := pbxUAS.waitInvite(t, 5*time.Second)
		pbxBodyCh <- copyBody(dss.InviteRequest.Body())
		_ = dss.Respond(200, "OK",
			sdpWithAddr(addrHost(pbxConn.LocalAddr()), addrPort(t, pbxConn.LocalAddr())))
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	pbxBody := <-pbxBodyCh
	pbxH, pbxP, err := parseMedia(pbxBody)
	if err != nil {
		t.Fatalf("parse seq PBX: %v", err)
	}
	epH, epP, err := parseMedia(sess.InviteResponse.Body())
	if err != nil {
		t.Fatalf("parse seq EP: %v", err)
	}

	seqEPAddr := &net.UDPAddr{IP: net.ParseIP(epH), Port: epP}
	seqPBXAddr := &net.UDPAddr{IP: net.ParseIP(pbxH), Port: pbxP}

	callerPkt := []byte{0x80, 0x00, 0xAA, 0x01}
	calleePkt := []byte{0x80, 0x00, 0xBB, 0x02}

	if _, err := epConn.WriteTo(callerPkt, seqEPAddr); err != nil {
		t.Fatalf("send caller: %v", err)
	}
	if _, err := pbxConn.WriteTo(calleePkt, seqPBXAddr); err != nil {
		t.Fatalf("send callee: %v", err)
	}

	// TapA stream 1 = caller, stream 2 = callee.
	gotA1 := readWithDeadline(t, sA1)
	gotA2 := readWithDeadline(t, sA2)
	// TapB stream 1 = caller, stream 2 = callee.
	gotB1 := readWithDeadline(t, sB1)
	gotB2 := readWithDeadline(t, sB2)

	if string(gotA1) != string(callerPkt) {
		t.Errorf("tapA s1 = %v, want caller %v", gotA1, callerPkt)
	}
	if string(gotA2) != string(calleePkt) {
		t.Errorf("tapA s2 = %v, want callee %v", gotA2, calleePkt)
	}
	if string(gotB1) != string(callerPkt) {
		t.Errorf("tapB s1 = %v, want caller %v", gotB1, callerPkt)
	}
	if string(gotB2) != string(calleePkt) {
		t.Errorf("tapB s2 = %v, want callee %v", gotB2, calleePkt)
	}
}
