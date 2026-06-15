//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Given an app that answers a provisional 183 before its 200; When a call arrives;
// Then the provisional is relayed to the caller ahead of the final answer.
func TestAppProvisionalRelayedToCaller(t *testing.T) {
	appA := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	go func() {
		dss := appA.waitInvite(t, 5*time.Second)
		_ = dss.Respond(183, "Session Progress", nil)
		time.Sleep(20 * time.Millisecond)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appA, "none", "skip")})
	s := startReady(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := caller.invite(ctx, "sip:"+s.sipListen, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	var sawProvisional bool
	err = sess.WaitAnswer(ctx, sipgo.AnswerOptions{
		OnResponse: func(res *sip.Response) error {
			if res.StatusCode == 183 {
				sawProvisional = true
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if !sawProvisional {
		t.Fatal("caller never saw the app's 183 provisional")
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected final 200, got %d", sess.InviteResponse.StatusCode)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}
}

// Given an unreachable app with on_failure: skip; When a call arrives; Then the
// dead leg is skipped and the call still connects via the PBX, with the failure
// counted.
func TestUnreachableAppSkipped(t *testing.T) {
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{
		{Name: "dead", URI: deadAddr(t), Media: "none", OnFailure: "skip"},
	})
	s := startReady(t, cfg)

	establish(t, caller, s.sipListen)

	_, body := mustGet(t, "http://"+s.obsListen+"/metrics")
	if !strings.Contains(body, `sequencer_app_failures_total{app="dead"} 1`) {
		t.Fatalf("metrics missing failure counter for unreachable app\nbody:\n%s", body)
	}
}

// Given an unreachable app with on_failure: abort; When a call arrives; Then the
// call is rejected (503) and the PBX is never invited.
func TestUnreachableAppAbortsCall(t *testing.T) {
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	cfg := baseConfig(t, pbx, []yamlApp{
		{Name: "dead", URI: deadAddr(t), Media: "none", OnFailure: "abort"},
	})
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
		t.Fatalf("expected ErrDialogResponse from aborted call, got %v", err)
	}
	if dialErr.Res.StatusCode != 503 {
		t.Fatalf("expected 503 Service Unavailable, got %d", dialErr.Res.StatusCode)
	}

	pbx.noInvite(t, 300*time.Millisecond)
}

// Given two tap-mode apps in the chain; When the caller sends RTP; Then each app
// independently receives a forked copy of the caller direction on its first stream.
func TestMultipleTapAppsEachForked(t *testing.T) {
	tap1 := newFakeUAS(t)
	tap2 := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	t1s1, _, t1s1Port := udpSocket(t)
	_, _, t1s2Port := udpSocket(t) // callee-direction receiver; port only
	t2s1, _, t2s1Port := udpSocket(t)
	_, _, t2s2Port := udpSocket(t)
	epConn, epHost, epPort := udpSocket(t)
	_, pbxHost, pbxPort := udpSocket(t)

	go func() {
		dss := tap1.waitInvite(t, 5*time.Second)
		_ = dss.Respond(200, "OK", tapAnswerSDP("127.0.0.1", t1s1Port, t1s2Port))
	}()
	go func() {
		dss := tap2.waitInvite(t, 5*time.Second)
		_ = dss.Respond(200, "OK", tapAnswerSDP("127.0.0.1", t2s1Port, t2s2Port))
	}()
	autoAnswer(t, pbx, sdpWithAddr(pbxHost, pbxPort))

	cfg := baseConfig(t, pbx, []yamlApp{
		app("tap1", tap1, "tap", "skip"),
		app("tap2", tap2, "tap", "skip"),
	})
	// Two tap apps need more port pairs: endpoint + PBX anchors + two pairs per tap.
	cfg.RTPRange = freeRTPRangeSpan(t, 40)
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
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	epAnchorHost, epAnchorPort := parseAudioConn(t, sess.InviteResponse.Body())

	up := []byte{0x80, 0x00, 0xDE, 0xAD}
	sendUDP(t, epConn, epAnchorHost, epAnchorPort, up)

	if got := recvUDP(t, t1s1, 2*time.Second); !bytes.Equal(got, up) {
		t.Fatalf("tap1 stream 1: got %v, want %v", got, up)
	}
	if got := recvUDP(t, t2s1, 2*time.Second); !bytes.Equal(got, up) {
		t.Fatalf("tap2 stream 1: got %v, want %v", got, up)
	}
}
