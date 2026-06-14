//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// app builds a yamlApp for a fake app server with the given policy and media mode.
func app(name string, f *fakeUAS, media, onFailure string) yamlApp {
	return yamlApp{Name: name, URI: f.sipURI(), Media: media, OnFailure: onFailure}
}

// Given a sequence [A,B,C]; When a call arrives; Then every app is invited exactly
// once in configured order before the PBX, and the call connects.
func TestChainTraversesApplicationsInOrder(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	appC := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	log := newChainLog()
	serveApp(t, appA, "A", log, 200, "OK", []byte(testSDP2))
	serveApp(t, appB, "B", log, 200, "OK", []byte(testSDP2))
	serveApp(t, appC, "C", log, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{
		app("A", appA, "none", "skip"),
		app("B", appB, "none", "skip"),
		app("C", appC, "none", "skip"),
	})
	s := startReady(t, cfg)

	establish(t, caller, s.sipListen)

	got := log.snapshot()
	want := []string{"A", "B", "C"}
	if !equalOrder(got, want) {
		t.Fatalf("app visit order = %v, want %v", got, want)
	}
}

// Given an app with on_failure: skip that rejects; When a call arrives; Then the
// chain continues and the call still connects, and the app's failure is counted.
func TestAppFailureSkipContinuesChain(t *testing.T) {
	appA := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	serveApp(t, appA, "A", nil, 486, "Busy Here", nil) // rejects, policy skip
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appA, "none", "skip")})
	s := startReady(t, cfg)

	// Call still connects: the rejecting app is skipped, the PBX answers.
	establish(t, caller, s.sipListen)

	_, body := mustGet(t, "http://"+s.obsListen+"/metrics")
	if !strings.Contains(body, `sequencer_app_failures_total{app="A"} 1`) {
		t.Fatalf("metrics missing app failure counter for A\nbody:\n%s", body)
	}
}

// Given an app with on_failure: abort that rejects 486; When a call arrives; Then
// the caller sees the rejection, the PBX is never invited, and no call survives.
func TestAppFailureAbortRejectsCall(t *testing.T) {
	appA := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	serveApp(t, appA, "A", nil, 486, "Busy Here", nil) // rejects, policy abort

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appA, "none", "abort")})
	s := startReady(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := caller.invite(ctx, "sip:"+s.sipListen, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	err = sess.WaitAnswer(ctx, sipgo.AnswerOptions{})
	var dialErr *sipgo.ErrDialogResponse
	if !errors.As(err, &dialErr) {
		t.Fatalf("expected ErrDialogResponse from aborted chain, got %v", err)
	}
	if dialErr.Res.StatusCode != 486 {
		t.Fatalf("expected 486 propagated to caller, got %d", dialErr.Res.StatusCode)
	}

	pbx.noInvite(t, 300*time.Millisecond)

	if status, body := mustGet(t, "http://"+s.obsListen+"/metrics"); status == 200 {
		if !strings.Contains(body, "sequencer_active_calls 0") {
			t.Fatalf("expected 0 active calls after abort\nbody:\n%s", body)
		}
	}
}

// Given a two-app chain; When a call arrives; Then each app's INVITE carries the
// sequencer correlation headers: the same X-Sequencer-Call-Id across legs and a
// distinct X-Sequencer-Leg-Id per leg.
func TestAppsReceiveSequencerHeaders(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	log := newChainLog()
	serveApp(t, appA, "A", log, 200, "OK", []byte(testSDP2))
	serveApp(t, appB, "B", log, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{
		app("A", appA, "none", "skip"),
		app("B", appB, "none", "skip"),
	})
	s := startReady(t, cfg)

	establish(t, caller, s.sipListen)

	reqA := log.request("A")
	reqB := log.request("B")
	if reqA == nil || reqB == nil {
		t.Fatalf("missing captured INVITE: A=%v B=%v", reqA, reqB)
	}

	callA := headerValue(t, reqA, "X-Sequencer-Call-Id")
	callB := headerValue(t, reqB, "X-Sequencer-Call-Id")
	if callA == "" {
		t.Fatal("app A INVITE missing X-Sequencer-Call-Id")
	}
	if callA != callB {
		t.Fatalf("X-Sequencer-Call-Id differs across legs: A=%q B=%q", callA, callB)
	}

	legA := headerValue(t, reqA, "X-Sequencer-Leg-Id")
	legB := headerValue(t, reqB, "X-Sequencer-Leg-Id")
	if legA == "" || legB == "" {
		t.Fatalf("missing X-Sequencer-Leg-Id: A=%q B=%q", legA, legB)
	}
	if legA == legB {
		t.Fatalf("X-Sequencer-Leg-Id not distinct per leg: %q", legA)
	}
}

// Given a successful two-app chain; When /metrics is scraped; Then each app has an
// invocation counted.
func TestAppInvocationMetrics(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	serveApp(t, appA, "A", nil, 200, "OK", []byte(testSDP2))
	serveApp(t, appB, "B", nil, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{
		app("A", appA, "none", "skip"),
		app("B", appB, "none", "skip"),
	})
	s := startReady(t, cfg)

	establish(t, caller, s.sipListen)

	_, body := mustGet(t, "http://"+s.obsListen+"/metrics")
	for _, name := range []string{"A", "B"} {
		want := fmt.Sprintf(`sequencer_app_invocations_total{app=%q} 1`, name)
		if !strings.Contains(body, want) {
			t.Fatalf("metrics missing %q\nbody:\n%s", want, body)
		}
	}
}

// Given a tap-mode app and an established call; When RTP flows each way; Then the
// app receives a forked copy of both directions — caller direction on its first
// stream, callee direction on its second — while the call's own relay continues.
func TestTapAppReceivesForkedMedia(t *testing.T) {
	tapApp := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	// The app's two recvonly tap-stream receivers.
	stream1, _, s1Port := udpSocket(t)
	stream2, _, s2Port := udpSocket(t)
	// The call's own endpoint and PBX media sockets.
	epConn, epHost, epPort := udpSocket(t)
	pbxConn, pbxHost, pbxPort := udpSocket(t)

	// Tap app answers the dual-stream offer with its two receiver ports.
	go func() {
		dss := tapApp.waitInvite(t, 5*time.Second)
		_ = dss.Respond(200, "OK", tapAnswerSDP("127.0.0.1", s1Port, s2Port))
	}()

	var pbxOffer []byte
	pbxDone := make(chan struct{})
	go func() {
		dss := pbx.waitInvite(t, 5*time.Second)
		pbxOffer = dss.InviteRequest.Body()
		_ = dss.Respond(200, "OK", sdpWithAddr(pbxHost, pbxPort))
		close(pbxDone)
	}()

	cfg := baseConfig(t, pbx, []yamlApp{app("tap", tapApp, "tap", "skip")})
	s := startReady(t, cfg)

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

	epAnchorHost, epAnchorPort := parseAudioConn(t, sess.InviteResponse.Body())
	pbxAnchorHost, pbxAnchorPort := parseAudioConn(t, pbxOffer)

	// Caller direction: endpoint → engine → PBX, forked to tap stream 1.
	up := []byte{0x80, 0x00, 0xCA, 0xFE}
	sendUDP(t, epConn, epAnchorHost, epAnchorPort, up)
	if got := recvUDP(t, pbxConn, 2*time.Second); !bytes.Equal(got, up) {
		t.Fatalf("endpoint→PBX relay: got %v, want %v", got, up)
	}
	if got := recvUDP(t, stream1, 2*time.Second); !bytes.Equal(got, up) {
		t.Fatalf("tap stream 1 (caller direction): got %v, want %v", got, up)
	}

	// Callee direction: PBX → engine → endpoint, forked to tap stream 2.
	down := []byte{0x80, 0x00, 0xBE, 0xEF}
	sendUDP(t, pbxConn, pbxAnchorHost, pbxAnchorPort, down)
	if got := recvUDP(t, epConn, 2*time.Second); !bytes.Equal(got, down) {
		t.Fatalf("PBX→endpoint relay: got %v, want %v", got, down)
	}
	if got := recvUDP(t, stream2, 2*time.Second); !bytes.Equal(got, down) {
		t.Fatalf("tap stream 2 (callee direction): got %v, want %v", got, down)
	}
}

// ── chain helpers ────────────────────────────────────────────────────────────

// headerValue returns the value of the named header on req, or "" if absent.
func headerValue(t *testing.T, req *sip.Request, name string) string {
	t.Helper()
	h := req.GetHeader(name)
	if h == nil {
		return ""
	}
	return h.Value()
}

func equalOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
