//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// PRD §5: a mid-call hold (re-INVITE with a=sendonly) propagates through the
// existing PBX leg with its direction attribute intact; the chain is not re-run.
func TestHoldReInvitePropagatesToPBX(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	// App answers exactly its one INVITE; a re-run would deliver a second.
	go func() {
		dss := appSrv.waitInvite(t, 5*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()

	// PBX answers the initial INVITE, then captures + answers the hold re-INVITE.
	holdBody := make(chan []byte, 1)
	go func() {
		dss1 := pbx.waitInvite(t, 5*time.Second)
		_ = dss1.Respond(200, "OK", sdpWithAddr("127.0.0.1", 30000))
		dss2 := pbx.waitInvite(t, 5*time.Second)
		holdBody <- dss2.InviteRequest.Body()
		_ = dss2.Respond(200, "OK", sdpWithAddr("127.0.0.1", 30000))
	}()

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "none", "skip")})
	s := startReady(t, cfg)

	sess := establishWithOffer(t, caller, "sip:"+s.sipListen, sdpWithAddr("127.0.0.1", 40000))

	// Caller holds: re-INVITE with a=sendonly to the engine's Contact.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reInvite := sip.NewRequest(sip.INVITE, sess.InviteResponse.Contact().Address)
	reInvite.SetBody(append(sdpWithAddr("127.0.0.1", 40002), []byte("a=sendonly\r\n")...))
	reInvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	res, err := sess.Do(ctx, reInvite)
	if err != nil {
		t.Fatalf("caller hold re-INVITE: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("hold re-INVITE answer: expected 200, got %d", res.StatusCode)
	}

	// The PBX leg received the hold with its direction attribute forwarded verbatim.
	select {
	case body := <-holdBody:
		if !bytes.Contains(body, []byte("a=sendonly")) {
			t.Fatalf("PBX re-INVITE did not carry the hold direction:\n%s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PBX never received the hold re-INVITE")
	}

	// Chain not re-run: the app gets no second INVITE.
	appSrv.noInvite(t, 400*time.Millisecond)
}

// PRD §5: a caller that CANCELs before answer aborts the call — the INVITE is not
// completed and the engine tears down any legs it had started (no active call).
func TestCallerCancelBeforeAnswerTearsDown(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	// Neither app nor PBX answers: the call stays in setup, so CANCEL is valid.
	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "none", "skip")})
	s := startReady(t, cfg)

	// sipgo sends CANCEL when the WaitAnswer context is cancelled mid-setup.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := caller.invite(ctx, "sip:"+s.sipListen, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	// Let the engine register the call and start dialing the app, then cancel.
	time.AfterFunc(300*time.Millisecond, cancel)

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err == nil {
		t.Fatal("expected CANCEL to abort the call, got a successful answer")
	}

	// The engine tears the call down: active calls return to zero.
	waitActiveCalls(t, s.obsListen, 0, 3*time.Second)
}

// waitActiveCalls polls /metrics until sequencer_active_calls equals want.
func waitActiveCalls(t *testing.T, obsListen string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	target := "sequencer_active_calls " + strconv.Itoa(want)
	var last string
	for time.Now().Before(deadline) {
		_, body := mustGet(t, "http://"+obsListen+"/metrics")
		if strings.Contains(body, target) {
			return
		}
		last = body
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("sequencer_active_calls never reached %d within %s\nlast metrics:\n%s", want, timeout, last)
}
