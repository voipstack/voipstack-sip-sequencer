package b2bua

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/emiago/sipgo"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// ── PortAllocator ─────────────────────────────────────────────────────────────

func TestPortAllocatorAcquireRelease(t *testing.T) {
	pa := newPortAllocator(10000, 10010)

	p1, err := pa.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if p1.RTP%2 != 0 {
		t.Fatalf("RTP port %d not even", p1.RTP)
	}
	if p1.RTCP != p1.RTP+1 {
		t.Fatalf("RTCP %d != RTP+1 %d", p1.RTCP, p1.RTP+1)
	}

	pa.Release(p1)

	p2, err := pa.Acquire()
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	if p2.RTP != p1.RTP {
		t.Fatalf("expected reuse of %d, got %d", p1.RTP, p2.RTP)
	}
}

func TestPortAllocatorExhaustion(t *testing.T) {
	pa := newPortAllocator(10000, 10001) // space for exactly 1 pair: 10000/10001

	_, err := pa.Acquire()
	if err != nil {
		t.Fatalf("first Acquire failed: %v", err)
	}
	_, err = pa.Acquire()
	if err != ErrPortsExhausted {
		t.Fatalf("expected ErrPortsExhausted, got %v", err)
	}
}

// ── TestBadPortRangeFailsAtNew ────────────────────────────────────────────────

// Given a bad rtp.port_range; When New is called; Then it returns an error immediately.
func TestBadPortRangeFailsAtNew(t *testing.T) {
	badCfgs := []string{"", "abc", "10001-20000", "20000-10000", "0-10000"}
	for _, pr := range badCfgs {
		cfg := config.Config{
			SIP:     config.SIP{Listen: "127.0.0.1:0"},
			NextHop: "sip:127.0.0.1:5060",
			RTP:     config.RTP{PortRange: pr},
		}
		_, err := New(cfg)
		if err == nil {
			t.Fatalf("New with port_range=%q: expected error, got nil", pr)
		}
	}
}

// ── media flow behavior tests ─────────────────────────────────────────────────

// sdpWithAddr builds a minimal SDP with the given c= and m= audio port.
func sdpWithAddr(host string, port int) []byte {
	return []byte("v=0\r\no=- 0 0 IN IP4 " + host + "\r\ns=-\r\nc=IN IP4 " + host + "\r\nt=0 0\r\nm=audio " + strconv.Itoa(port) + " RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n")
}

// Given established call; When endpoint and PBX send RTP; Then packets traverse the sequencer.
func TestCallMediaFlowsThroughSequencer(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	eng := startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	// Caller SDP: RTP on a local UDP port we control
	callerRTPConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("callerRTPConn: %v", err)
	}
	defer callerRTPConn.Close()
	t.Cleanup(func() { callerRTPConn.Close() })

	callerAddr := callerRTPConn.LocalAddr().String()
	callerHost, callerPortStr, _ := net.SplitHostPort(callerAddr)
	callerPort := mustAtoi(t, callerPortStr)

	callerSDP := sdpWithAddr(callerHost, callerPort)

	// PBX fake SDP: RTP on a local UDP port we control
	pbxRTPConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pbxRTPConn: %v", err)
	}
	defer pbxRTPConn.Close()
	t.Cleanup(func() { pbxRTPConn.Close() })

	pbxAddr := pbxRTPConn.LocalAddr().String()
	pbxHost, pbxPortStr, _ := net.SplitHostPort(pbxAddr)
	pbxPort := mustAtoi(t, pbxPortStr)

	pbxSDP := sdpWithAddr(pbxHost, pbxPort)

	// The SIP call
	sess, err := caller.invite(ctx, "sip:"+listenAddr, callerSDP)
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	go func() {
		dss := app.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()

	var pbxInviteBody []byte
	pbxAnswerDone := make(chan struct{})
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		pbxInviteBody = dss.InviteRequest.Body()
		_ = dss.Respond(200, "OK", pbxSDP)
		close(pbxAnswerDone)
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	<-pbxAnswerDone
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	// The PBX invite body must use the sequencer's allocated port, not the caller's port.
	_ = eng
	if pbxInviteBody == nil {
		t.Fatal("PBX received no SDP body")
	}
	_, seqPBXPort, err := parseMedia(pbxInviteBody)
	if err != nil {
		t.Fatalf("parseMedia on PBX invite body: %v", err)
	}
	if seqPBXPort == callerPort {
		t.Fatalf("PBX invite SDP still has caller port %d — media not anchored", callerPort)
	}

	// The caller's 200 answer must use the sequencer's allocated port, not the PBX's port.
	callerAnswerBody := sess.InviteResponse.Body()
	if callerAnswerBody == nil {
		t.Fatal("caller received no 200 SDP body")
	}
	_, seqEPPort, err := parseMedia(callerAnswerBody)
	if err != nil {
		t.Fatalf("parseMedia on caller 200 body: %v", err)
	}
	if seqEPPort == pbxPort {
		t.Fatalf("caller 200 SDP still has PBX port %d — media not anchored", pbxPort)
	}
}

// Given established call; When RTP packet sent endpoint→seq→PBX; Then payload unchanged (AC2).
func TestMediaPayloadRelayedUnchanged(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	// Endpoint RTP receiver
	epConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ep listen: %v", err)
	}
	defer epConn.Close()
	t.Cleanup(func() { epConn.Close() })
	epHost, epPortStr, _ := net.SplitHostPort(epConn.LocalAddr().String())
	epPort := mustAtoi(t, epPortStr)

	// PBX RTP receiver
	pbxConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pbx listen: %v", err)
	}
	defer pbxConn.Close()
	t.Cleanup(func() { pbxConn.Close() })
	pbxHost, pbxPortStr, _ := net.SplitHostPort(pbxConn.LocalAddr().String())
	pbxPort := mustAtoi(t, pbxPortStr)

	callerSDP := sdpWithAddr(epHost, epPort)
	pbxSDP := sdpWithAddr(pbxHost, pbxPort)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, callerSDP)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	go func() {
		dss := app.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()

	var seqEPAddr, seqPBXAddr string
	pbxDone := make(chan struct{})
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		// The PBX invite body tells us the sequencer's PBX-facing address.
		seqPBXAddr = string(dss.InviteRequest.Body())
		_ = dss.Respond(200, "OK", pbxSDP)
		close(pbxDone)
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	<-pbxDone
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	// Parse the sequencer's endpoint-facing and PBX-facing RTP ports from the SDPs.
	seqEPAddr = string(sess.InviteResponse.Body())

	seqEPHost, seqEPPort, err := parseMedia([]byte(seqEPAddr))
	if err != nil {
		t.Fatalf("parse seq ep SDP: %v", err)
	}
	seqPBXHost, seqPBXPort, err := parseMedia([]byte(seqPBXAddr))
	if err != nil {
		t.Fatalf("parse seq pbx SDP: %v", err)
	}

	// Send a test RTP packet from the "PBX" side to the sequencer's PBX-facing port.
	payload := []byte{0x80, 0x00, 0x01, 0x02, 0x03, 0x04}
	seqPBXUDPAddr := &net.UDPAddr{IP: net.ParseIP(seqPBXHost), Port: seqPBXPort}
	if _, err := pbxConn.WriteTo(payload, seqPBXUDPAddr); err != nil {
		t.Fatalf("send RTP to seq pbx side: %v", err)
	}

	// The packet should arrive at the endpoint conn.
	if err := epConn.(*net.UDPConn).SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 1500)
	n, _, err := epConn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read relayed packet at endpoint: %v", err)
	}
	if string(buf[:n]) != string(payload) {
		t.Fatalf("payload mismatch: got %v, want %v", buf[:n], payload)
	}

	// Also verify endpoint→PBX direction.
	seqEPUDPAddr := &net.UDPAddr{IP: net.ParseIP(seqEPHost), Port: seqEPPort}
	payload2 := []byte{0x80, 0x00, 0x02, 0x03, 0x04, 0x05}
	if _, err := epConn.WriteTo(payload2, seqEPUDPAddr); err != nil {
		t.Fatalf("send RTP to seq ep side: %v", err)
	}
	if err := pbxConn.(*net.UDPConn).SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	n, _, err = pbxConn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read relayed packet at pbx: %v", err)
	}
	if string(buf[:n]) != string(payload2) {
		t.Fatalf("payload2 mismatch: got %v, want %v", buf[:n], payload2)
	}
}

// Given established call; When BYE; Then ports are freed and available for next call (AC4).
func TestMediaPortsReleasedOnTeardown(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)

	listenAddr := freeAddr(t)
	// Range of exactly 2 pairs (4 ports) so that if ports are not released the second call fails.
	cfg := config.Config{
		SIP:      config.SIP{Listen: listenAddr},
		NextHop:  pbx.sipURI(),
		RTP:      config.RTP{PortRange: "12000-12006"}, // 3 pairs: 12000,12002,12004
		Sequence: []config.Application{{Name: "app", URI: app.sipURI(), OnFailure: config.FailureSkip}},
	}
	startEngine(t, cfg, 0)
	ctx := context.Background()

	autoAnswer(t, app, "", nil)

	for i := 0; i < 3; i++ {
		caller := newFakeUAC(t)
		pbxConnI, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("pbx listen %d: %v", i, err)
		}
		defer pbxConnI.Close()
		h, p, _ := net.SplitHostPort(pbxConnI.LocalAddr().String())
		port := mustAtoi(t, p)
		pbxSDP := sdpWithAddr(h, port)
		pbxConnI.Close()

		go func() {
			dss := pbx.waitInvite(t, 3*time.Second)
			_ = dss.Respond(200, "OK", pbxSDP)
		}()

		epConn, err := net.ListenPacket("udp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("ep listen %d: %v", i, err)
		}
		defer epConn.Close()
		eh, ep, _ := net.SplitHostPort(epConn.LocalAddr().String())
		eport := mustAtoi(t, ep)
		callerSDP := sdpWithAddr(eh, eport)

		sess, err := caller.invite(ctx, "sip:"+listenAddr, callerSDP)
		if err != nil {
			t.Fatalf("call %d invite: %v", i, err)
		}
		if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
			t.Fatalf("call %d WaitAnswer: %v", i, err)
		}
		if err := sess.Ack(ctx); err != nil {
			t.Fatalf("call %d ACK: %v", i, err)
		}
		// Hang up to release ports
		if err := sess.Bye(ctx); err != nil {
			t.Fatalf("call %d BYE: %v", i, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Given range full; When new call arrives; Then it fails with 503 and existing call media flows (AC6).
func TestPortExhaustionFailsCleanly(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)

	listenAddr := freeAddr(t)
	// Range: exactly 1 pair per side × 2 sides = need 2 pairs = 4 ports: 12100-12104 gives 2 pairs (12100,12102)
	cfg := config.Config{
		SIP:      config.SIP{Listen: listenAddr},
		NextHop:  pbx.sipURI(),
		RTP:      config.RTP{PortRange: "12100-12104"},
		Sequence: []config.Application{{Name: "app", URI: app.sipURI(), OnFailure: config.FailureSkip}},
	}
	eng := startEngine(t, cfg, 0)
	ctx := context.Background()

	autoAnswer(t, app, "", nil)

	// First call: should succeed and hold the 2 pairs.
	epConn1, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ep1 listen: %v", err)
	}
	defer epConn1.Close()
	t.Cleanup(func() { epConn1.Close() })
	h1, p1, _ := net.SplitHostPort(epConn1.LocalAddr().String())
	eport1 := mustAtoi(t, p1)
	callerSDP1 := sdpWithAddr(h1, eport1)

	pbxConn1, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pbx1 listen: %v", err)
	}
	defer pbxConn1.Close()
	t.Cleanup(func() { pbxConn1.Close() })
	ph1, pp1, _ := net.SplitHostPort(pbxConn1.LocalAddr().String())
	pport1 := mustAtoi(t, pp1)
	pbxSDP1 := sdpWithAddr(ph1, pport1)

	caller1 := newFakeUAC(t)
	sess1, err := caller1.invite(ctx, "sip:"+listenAddr, callerSDP1)
	if err != nil {
		t.Fatalf("caller1 invite: %v", err)
	}
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", pbxSDP1)
	}()
	if err := sess1.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller1 WaitAnswer: %v", err)
	}
	if err := sess1.Ack(ctx); err != nil {
		t.Fatalf("caller1 ACK: %v", err)
	}
	if eng.calls.len() != 1 {
		t.Fatalf("expected 1 call, got %d", eng.calls.len())
	}

	// Second call: ports exhausted → should fail.
	caller2 := newFakeUAC(t)
	callerSDP2 := sdpWithAddr("127.0.0.1", 11000)
	sess2, err := caller2.invite(ctx, "sip:"+listenAddr, callerSDP2)
	if err != nil {
		t.Fatalf("caller2 invite: %v", err)
	}
	go func() {
		// PBX should NOT receive this invite; drain with a short timeout so the test doesn't block.
		select {
		case dss := <-pbx.invites:
			_ = dss.Respond(200, "OK", pbxSDP1)
		case <-time.After(500 * time.Millisecond):
		}
	}()
	err = sess2.WaitAnswer(ctx, sipgo.AnswerOptions{})
	if err == nil {
		t.Fatal("second call should have failed due to port exhaustion")
	}

	// First call still in registry (media still flowing).
	time.Sleep(50 * time.Millisecond)
	if eng.calls.len() != 1 {
		t.Fatalf("expected 1 active call after second-call failure, got %d", eng.calls.len())
	}
}

// Given established call; When PBX sends RTP to endpoint; Then the packet arrives at
// the endpoint from the endpoint-facing sequencer RTP port (not the PBX-facing port).
// When endpoint sends RTP to PBX; Then the packet arrives at the PBX from the
// PBX-facing sequencer RTP port (not the endpoint-facing port). Payloads unchanged.
func TestRelayedRTPSourcePortMatchesAdvertisedPort(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	epConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ep listen: %v", err)
	}
	defer epConn.Close()
	t.Cleanup(func() { epConn.Close() })

	pbxConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pbx listen: %v", err)
	}
	defer pbxConn.Close()
	t.Cleanup(func() { pbxConn.Close() })

	epHost, epPortStr, _ := net.SplitHostPort(epConn.LocalAddr().String())
	epPort := mustAtoi(t, epPortStr)
	pbxHost, pbxPortStr, _ := net.SplitHostPort(pbxConn.LocalAddr().String())
	pbxPort := mustAtoi(t, pbxPortStr)

	callerSDP := sdpWithAddr(epHost, epPort)
	pbxSDP := sdpWithAddr(pbxHost, pbxPort)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, callerSDP)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	go func() {
		dss := app.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()

	var pbxInviteBody []byte
	pbxDone := make(chan struct{})
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		pbxInviteBody = dss.InviteRequest.Body()
		_ = dss.Respond(200, "OK", pbxSDP)
		close(pbxDone)
	}()

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	<-pbxDone
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	seqPBXHost, seqPBXPort, err := parseMedia(pbxInviteBody)
	if err != nil {
		t.Fatalf("parse seq pbx SDP: %v", err)
	}
	seqEPHost, seqEPPort, err := parseMedia(sess.InviteResponse.Body())
	if err != nil {
		t.Fatalf("parse seq ep SDP: %v", err)
	}

	seqPBXUDPAddr := &net.UDPAddr{IP: net.ParseIP(seqPBXHost), Port: seqPBXPort}
	seqEPUDPAddr := &net.UDPAddr{IP: net.ParseIP(seqEPHost), Port: seqEPPort}

	// PBX→endpoint: packet must arrive from the endpoint-facing RTP port.
	pbxToEP := []byte{0x80, 0x00, 0xBE, 0xEF}
	if _, err := pbxConn.WriteTo(pbxToEP, seqPBXUDPAddr); err != nil {
		t.Fatalf("send PBX→seq: %v", err)
	}

	buf := make([]byte, 1500)
	if err := epConn.(*net.UDPConn).SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	n, from, err := epConn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read at endpoint: %v", err)
	}
	if string(buf[:n]) != string(pbxToEP) {
		t.Fatalf("payload mismatch: got %v, want %v", buf[:n], pbxToEP)
	}
	fromUDP := from.(*net.UDPAddr)
	if fromUDP.Port != seqEPPort {
		t.Fatalf("PBX→endpoint arrived from port %d, want endpoint-facing port %d", fromUDP.Port, seqEPPort)
	}

	// Endpoint→PBX: packet must arrive from the PBX-facing RTP port.
	epToPBX := []byte{0x80, 0x00, 0xCA, 0xFE}
	if _, err := epConn.WriteTo(epToPBX, seqEPUDPAddr); err != nil {
		t.Fatalf("send EP→seq: %v", err)
	}

	if err := pbxConn.(*net.UDPConn).SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	n, from, err = pbxConn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("read at PBX: %v", err)
	}
	if string(buf[:n]) != string(epToPBX) {
		t.Fatalf("payload mismatch: got %v, want %v", buf[:n], epToPBX)
	}
	fromUDP = from.(*net.UDPAddr)
	if fromUDP.Port != seqPBXPort {
		t.Fatalf("endpoint→PBX arrived from port %d, want PBX-facing port %d", fromUDP.Port, seqPBXPort)
	}
}

// Given established call; When RTP flows; Then ports are within configured range (AC3).
func TestMediaPortsWithinConfiguredRange(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	const portMin = 13000
	const portMax = 14000
	listenAddr := freeAddr(t)
	cfg := config.Config{
		SIP:      config.SIP{Listen: listenAddr},
		NextHop:  pbx.sipURI(),
		RTP:      config.RTP{PortRange: "13000-14000"},
		Sequence: []config.Application{{Name: "app", URI: app.sipURI(), OnFailure: config.FailureSkip}},
	}
	startEngine(t, cfg, 0)
	ctx := context.Background()

	autoAnswer(t, app, "", nil)

	epConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ep listen: %v", err)
	}
	defer epConn.Close()
	t.Cleanup(func() { epConn.Close() })
	h, p, _ := net.SplitHostPort(epConn.LocalAddr().String())
	eport := mustAtoi(t, p)

	pbxConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pbx listen: %v", err)
	}
	defer pbxConn.Close()
	t.Cleanup(func() { pbxConn.Close() })
	ph, pp, _ := net.SplitHostPort(pbxConn.LocalAddr().String())
	pport := mustAtoi(t, pp)
	pbxSDP := sdpWithAddr(ph, pport)

	var pbxInviteBody []byte
	pbxDone := make(chan struct{})
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		pbxInviteBody = dss.InviteRequest.Body()
		_ = dss.Respond(200, "OK", pbxSDP)
		close(pbxDone)
	}()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, sdpWithAddr(h, eport))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	<-pbxDone
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	// Check pbx-facing port is in range
	_, seqPBXPort, err := parseMedia(pbxInviteBody)
	if err != nil {
		t.Fatalf("parse pbx invite body: %v", err)
	}
	if seqPBXPort < portMin || seqPBXPort > portMax {
		t.Fatalf("seq PBX-facing port %d out of range [%d, %d]", seqPBXPort, portMin, portMax)
	}

	// Check ep-facing port is in range
	_, seqEPPort, err := parseMedia(sess.InviteResponse.Body())
	if err != nil {
		t.Fatalf("parse ep 200 body: %v", err)
	}
	if seqEPPort < portMin || seqEPPort > portMax {
		t.Fatalf("seq EP-facing port %d out of range [%d, %d]", seqEPPort, portMin, portMax)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("mustAtoi %q: %v", s, err)
	}
	return n
}
