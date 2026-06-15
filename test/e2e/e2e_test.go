//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
)

// binPath is the compiled sip-sequencer binary, built once by TestMain and shared
// across all scenarios.
var binPath string

func TestMain(m *testing.M) {
	path, cleanup, err := buildBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build binary: %v\n", err)
		os.Exit(1)
	}
	binPath = path
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// baseConfig builds a config with the given app sequence and a UDP PBX next hop,
// picking fresh ports for the subprocess-owned listeners.
func baseConfig(t *testing.T, pbx *fakeUAS, apps []yamlApp) yamlConfig {
	t.Helper()
	return baseConfigURI(t, pbx.sipURI(), apps)
}

// baseConfigURI is baseConfig with an explicit next-hop URI (e.g. a proxy fake or a
// dead address), for tests that do not use a *fakeUAS PBX.
func baseConfigURI(t *testing.T, nextHopURI string, apps []yamlApp) yamlConfig {
	t.Helper()
	return yamlConfig{
		SIPListen:        freeUDPPort(t),
		NextHopURI:       nextHopURI,
		NextHopTransport: "udp",
		RTPRange:         freeRTPRange(t),
		ObsListen:        freeTCPPort(t),
		LogLevel:         "debug",
		Apps:             apps,
	}
}

// singleAppConfig builds a config with one media:none TCP app leg.
func singleAppConfig(t *testing.T, app, pbx *fakeUAS) yamlConfig {
	t.Helper()
	return baseConfig(t, pbx, []yamlApp{{
		Name:      "app1",
		URI:       app.sipURI(),
		Media:     "none",
		OnFailure: "abort",
	}})
}

// establish drives a full caller INVITE → 200 → ACK against the running binary's
// UDP listener and returns the confirmed caller dialog.
func establish(t *testing.T, caller *fakeUAC, sipListen string) *sipgo.DialogClientSession {
	t.Helper()
	return establishTo(t, caller, "sip:"+sipListen)
}

// establishTo is establish against an explicit target URI (e.g. a ws/wss address
// with a transport param). app and pbx must already be auto-answering.
func establishTo(t *testing.T, caller *fakeUAC, targetURI string) *sipgo.DialogClientSession {
	t.Helper()
	return establishWithOffer(t, caller, targetURI, []byte(testSDP))
}

// establishWithOffer drives a full INVITE → 200 → ACK with a caller-supplied offer
// SDP (e.g. advertising a real RTP socket for media tests).
func establishWithOffer(t *testing.T, caller *fakeUAC, targetURI string, offer []byte) *sipgo.DialogClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := caller.invite(ctx, targetURI, offer)
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("caller: expected 200, got %d %s",
			sess.InviteResponse.StatusCode, sess.InviteResponse.Reason)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}
	return sess
}

// connectTo sends an INVITE and waits for the 200 OK but does NOT ACK. In-dialog
// requests route to the engine's dialog Contact, which is the UDP signaling
// address regardless of inbound transport — so a WS/WSS caller cannot ACK it (the
// engine does not rewrite Contact per transport). For those listeners, reaching a
// 200 OK end to end is the meaningful black-box assertion.
func connectTo(t *testing.T, caller *fakeUAC, targetURI string) *sipgo.DialogClientSession {
	t.Helper()
	return connectWithOffer(t, caller, targetURI, []byte(testSDP))
}

// connectWithOffer is connectTo with a caller-supplied offer SDP (e.g. advertising
// a real RTP socket for media tests). It waits for 200 OK and does not ACK.
func connectWithOffer(t *testing.T, caller *fakeUAC, targetURI string, offer []byte) *sipgo.DialogClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := caller.invite(ctx, targetURI, offer)
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d %s", sess.InviteResponse.StatusCode, sess.InviteResponse.Reason)
	}
	return sess
}

// Given an invalid config; When the binary starts; Then it exits non-zero fast and
// reports the validation error on stderr.
func TestStartupRejectsInvalidConfig(t *testing.T) {
	dir := t.TempDir()
	// Unknown key "bogus" — rejected by the strict parser (KnownFields(true)).
	raw := "" +
		"sip:\n  listen: \"127.0.0.1:5060\"\n" +
		"next_hop:\n  uri: \"sip:127.0.0.1:5070\"\n" +
		"rtp:\n  port_range: \"10000-10010\"\n" +
		"sequence:\n  - name: app1\n    uri: \"sip:127.0.0.1:5080\"\n" +
		"bogus: true\n"
	cfgPath := writeRawConfig(t, dir, raw)

	s := start(t, cfgPath, "", "")
	err := s.waitExit(t, 5*time.Second)

	if err == nil {
		t.Fatalf("expected non-zero exit, got clean exit\nstderr:\n%s", s.stderr.String())
	}
	if !strings.Contains(s.stderr.String(), "error:") {
		t.Fatalf("stderr missing validation error\nstderr:\n%s", s.stderr.String())
	}
}

// Given the binary started with one app and a PBX; When the caller places a call;
// Then it connects end to end and the answer SDP carries the engine's anchored
// media port (not the PBX's), proving the B2BUA anchored media rather than passing
// the PBX SDP through.
func TestBinaryStartsAndConnectsCallEndToEnd(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	autoAnswer(t, app, []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := singleAppConfig(t, app, pbx)
	s := startReady(t, cfg)

	sess := establish(t, caller, s.sipListen)

	host, port := parseAudioConn(t, sess.InviteResponse.Body())
	if host != "127.0.0.1" {
		t.Fatalf("answer SDP host = %q, want engine host 127.0.0.1", host)
	}
	rtpMin, rtpMax := rangeBounds(t, cfg.RTPRange)
	if port < rtpMin || port > rtpMax {
		t.Fatalf("answer SDP media port = %d, not in engine RTP range %s — media not anchored",
			port, cfg.RTPRange)
	}
	if port == 9 {
		t.Fatal("answer SDP carries the PBX placeholder port 9 — media passed through, not anchored")
	}
}

// Given an established call; When the caller hangs up; Then the sequencer tears
// down both the app and PBX legs (both dialogs reach DialogStateEnded).
func TestCallerHangupTearsDownAllLegs(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	appEnded := autoAnswer(t, app, []byte(testSDP2))
	pbxEnded := autoAnswer(t, pbx, []byte(testSDP2))

	cfg := singleAppConfig(t, app, pbx)
	s := startReady(t, cfg)

	sess := establish(t, caller, s.sipListen)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.Bye(ctx); err != nil {
		t.Fatalf("caller BYE: %v", err)
	}

	waitEnded(t, "app", appEnded, 5*time.Second)
	waitEnded(t, "pbx", pbxEnded, 5*time.Second)
}

// Given an established call; When /metrics is scraped; Then the active-calls gauge
// reflects the live call.
func TestMetricsReflectsActiveCall(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	autoAnswer(t, app, []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := singleAppConfig(t, app, pbx)
	s := startReady(t, cfg)

	establish(t, caller, s.sipListen)

	status, body := mustGet(t, "http://"+s.obsListen+"/metrics")
	if status != 200 {
		t.Fatalf("/metrics status = %d", status)
	}
	if !strings.Contains(body, "sequencer_active_calls 1") {
		t.Fatalf("metrics missing active call gauge\nbody:\n%s", body)
	}
}

// Given an established call; When RTP is sent toward each anchored RTP port; Then
// the sequencer relays it byte-for-byte to the far leg in both directions —
// proving real media flows through the binary, not just signaling.
func TestMediaRelaysRTPThroughAnchoredPorts(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	// Real UDP sockets standing in for the caller's and PBX's RTP endpoints.
	epConn, epHost, epPort := udpSocket(t)
	pbxConn, pbxHost, pbxPort := udpSocket(t)

	autoAnswer(t, app, []byte(testSDP2))

	cfg := singleAppConfig(t, app, pbx)
	s := startReady(t, cfg)

	// The PBX learns the engine's PBX-facing anchored RTP address from the offer
	// the engine sends it; it answers with its own RTP socket.
	var pbxOffer []byte
	pbxDone := make(chan struct{})
	go func() {
		dss := pbx.waitInvite(t, 5*time.Second)
		pbxOffer = dss.InviteRequest.Body()
		_ = dss.Respond(200, "OK", sdpWithAddr(pbxHost, pbxPort))
		close(pbxDone)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := caller.invite(ctx, "sip:"+s.sipListen, sdpWithAddr(epHost, epPort))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	<-pbxDone
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	// Anchored addresses: endpoint-facing from the caller's 200, PBX-facing from
	// the offer the engine sent the PBX.
	epAnchorHost, epAnchorPort := parseAudioConn(t, sess.InviteResponse.Body())
	pbxAnchorHost, pbxAnchorPort := parseAudioConn(t, pbxOffer)

	// endpoint → engine → PBX
	payloadUp := []byte{0x80, 0x00, 0xCA, 0xFE}
	sendUDP(t, epConn, epAnchorHost, epAnchorPort, payloadUp)
	if got := recvUDP(t, pbxConn, 2*time.Second); !bytes.Equal(got, payloadUp) {
		t.Fatalf("endpoint→PBX relay: got %v, want %v", got, payloadUp)
	}

	// PBX → engine → endpoint
	payloadDown := []byte{0x80, 0x00, 0xBE, 0xEF}
	sendUDP(t, pbxConn, pbxAnchorHost, pbxAnchorPort, payloadDown)
	if got := recvUDP(t, epConn, 2*time.Second); !bytes.Equal(got, payloadDown) {
		t.Fatalf("PBX→endpoint relay: got %v, want %v", got, payloadDown)
	}
}

// ── UDP / SDP / metric helpers ───────────────────────────────────────────────

// udpSocket opens a UDP socket on 127.0.0.1 and returns it with its host and port.
func udpSocket(t *testing.T) (*net.UDPConn, string, int) {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("udp socket: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	host, portStr, _ := net.SplitHostPort(conn.LocalAddr().String())
	port, _ := strconv.Atoi(portStr)
	return conn, host, port
}

func sendUDP(t *testing.T, conn *net.UDPConn, host string, port int, payload []byte) {
	t.Helper()
	if _, err := conn.WriteToUDP(payload, &net.UDPAddr{IP: net.ParseIP(host), Port: port}); err != nil {
		t.Fatalf("send UDP to %s:%d: %v", host, port, err)
	}
}

func recvUDP(t *testing.T, conn *net.UDPConn, timeout time.Duration) []byte {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("recv UDP: %v", err)
	}
	return buf[:n]
}

// ── SDP / metric helpers ─────────────────────────────────────────────────────

// parseAudioConn extracts the connection host (c=IN IP4 <host>) and the audio
// media port (m=audio <port>) from an SDP body.
func parseAudioConn(t *testing.T, sdp []byte) (host string, port int) {
	t.Helper()
	for _, line := range strings.Split(string(sdp), "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "c=IN IP4 "):
			host = strings.TrimSpace(strings.TrimPrefix(line, "c=IN IP4 "))
		case strings.HasPrefix(line, "m=audio "):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				p, err := strconv.Atoi(fields[1])
				if err != nil {
					t.Fatalf("parse media port %q: %v", fields[1], err)
				}
				port = p
			}
		}
	}
	if host == "" || port == 0 {
		t.Fatalf("could not parse audio host/port from SDP:\n%s", sdp)
	}
	return host, port
}

func rangeBounds(t *testing.T, r string) (min, max int) {
	t.Helper()
	parts := strings.SplitN(r, "-", 2)
	if len(parts) != 2 {
		t.Fatalf("bad rtp range %q", r)
	}
	min, _ = strconv.Atoi(parts[0])
	max, _ = strconv.Atoi(parts[1])
	return min, max
}

func waitEnded(t *testing.T, leg string, ch <-chan struct{}, timeout time.Duration) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("%s leg not torn down within %s", leg, timeout)
	}
}
