package b2bua

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo"
)

// Given a MediaSession; markActivity refreshes the idle clock and idleFor measures the
// elapsed gap; a never-marked session reports zero (never idle).
func TestMediaSessionIdleTracking(t *testing.T) {
	m := &MediaSession{}
	if d := m.idleFor(time.Now()); d != 0 {
		t.Fatalf("unmarked session idleFor = %v, want 0", d)
	}
	m.markActivity()
	if d := m.idleFor(time.Now()); d > 50*time.Millisecond {
		t.Fatalf("just-marked session idleFor = %v, want ~0", d)
	}
	time.Sleep(120 * time.Millisecond)
	if d := m.idleFor(time.Now()); d < 100*time.Millisecond {
		t.Fatalf("idleFor after 120ms = %v, want >=100ms", d)
	}
	m.markActivity()
	if d := m.idleFor(time.Now()); d > 50*time.Millisecond {
		t.Fatalf("idleFor after re-mark = %v, want ~0", d)
	}
}

// Given an established call that exchanges no media (endpoint vanished without a BYE);
// When rtp.idle_timeout elapses; Then the reaper tears the call down and it leaves the
// registry (reclaiming its ports and relay goroutines).
func TestIdleCallReaped(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	cfg := testConfig(listenAddr, app.sipURI(), pbx.sipURI())
	cfg.RTP.IdleTimeoutDur = 400 * time.Millisecond
	eng := startEngine(t, cfg, 0)
	ctx := context.Background()

	autoAnswer(t, app, "", nil)
	autoAnswer(t, pbx, "", nil)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}
	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call, got %d", n)
	}

	// No media flows; the reaper must tear the call down within a few idle windows.
	deadline := time.Now().Add(3 * time.Second)
	for eng.calls.len() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle call was not reaped within 3s")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Given an established call that keeps exchanging RTP; When several idle windows pass;
// Then the call is NOT reaped — activity refreshes the idle clock (no false positives).
func TestActiveCallNotReaped(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	// Real endpoint and PBX RTP sockets so relayed writes have a live destination (no
	// spurious ICMP unreachable that could stop a relay direction during the test).
	epConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("ep listen: %v", err)
	}
	t.Cleanup(func() { epConn.Close() })
	epHost, epPortStr, _ := net.SplitHostPort(epConn.LocalAddr().String())
	epPort := mustAtoi(t, epPortStr)

	pbxConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pbx listen: %v", err)
	}
	t.Cleanup(func() { pbxConn.Close() })
	pbxHost, pbxPortStr, _ := net.SplitHostPort(pbxConn.LocalAddr().String())
	pbxPort := mustAtoi(t, pbxPortStr)

	listenAddr := freeAddr(t)
	cfg := testConfig(listenAddr, app.sipURI(), pbx.sipURI())
	cfg.RTP.IdleTimeoutDur = 600 * time.Millisecond
	eng := startEngine(t, cfg, 0)
	ctx := context.Background()

	autoAnswer(t, app, "", nil)
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", sdpWithAddr(pbxHost, pbxPort))
	}()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, sdpWithAddr(epHost, epPort))
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	// The sequencer's endpoint-facing anchor RTP port (from its 200 answer).
	_, seqEpPort, err := parseMedia(sess.InviteResponse.Body())
	if err != nil {
		t.Fatalf("parseMedia answer: %v", err)
	}
	seqEp := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: seqEpPort}
	rtp := []byte{0x80, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0xAA}

	// Feed RTP every 100ms for ~1.5s — well over two 600ms idle windows.
	for i := 0; i < 15; i++ {
		if _, err := epConn.WriteTo(rtp, seqEp); err != nil {
			t.Fatalf("send RTP into anchor: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if n := eng.calls.len(); n != 1 {
		t.Fatalf("active call was reaped despite continuous RTP: calls=%d", n)
	}
}
