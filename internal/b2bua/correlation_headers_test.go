package b2bua

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/sipgo"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// inviteCapture holds correlation headers and the SIP Call-ID captured from one INVITE.
type inviteCapture struct {
	seqCallID string // X-Sequencer-Call-Id
	seqLegID  string // X-Sequencer-Leg-Id
	sipCallID string // SIP Call-ID (sipgo-managed)
}

// captureInvite blocks until uas receives one INVITE (or 5 s elapses), captures
// correlation + SIP Call-ID headers, responds with sdp, and returns the capture.
// Must be called from the test goroutine.
func captureInvite(t *testing.T, uas *fakeUAS, sdp []byte) inviteCapture {
	t.Helper()
	select {
	case dss := <-uas.invites:
		ic := inviteCapture{sipCallID: dss.InviteRequest.CallID().Value()}
		if h := dss.InviteRequest.GetHeader("X-Sequencer-Call-Id"); h != nil {
			ic.seqCallID = h.Value()
		}
		if h := dss.InviteRequest.GetHeader("X-Sequencer-Leg-Id"); h != nil {
			ic.seqLegID = h.Value()
		}
		_ = dss.Respond(200, "OK", sdp)
		return ic
	case <-time.After(5 * time.Second):
		t.Fatal("captureInvite: timeout waiting for INVITE")
		return inviteCapture{}
	}
}

// ── correlation header behavior tests ────────────────────────────────────────

// Given multi-app chain [A,B] + PBX; When call arrives; Then every outbound leg
// carries the same X-Sequencer-Call-Id (AC1).
func TestSameCallIDOnEveryLeg(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "A", URI: appA.sipURI(), OnFailure: config.FailureSkip},
		{Name: "B", URI: appB.sipURI(), OnFailure: config.FailureSkip},
	}), 0)
	ctx := context.Background()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	// Bridge sends A → B → PBX sequentially; capture from test goroutine in order.
	capA := captureInvite(t, appA, []byte(testSDP2))
	capB := captureInvite(t, appB, []byte(testSDP2))
	capPBX := captureInvite(t, pbx, []byte(testSDP2))

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	if capA.seqCallID == "" {
		t.Fatal("appA missing X-Sequencer-Call-Id")
	}
	if capA.seqCallID != capB.seqCallID {
		t.Errorf("appA call_id %q != appB call_id %q", capA.seqCallID, capB.seqCallID)
	}
	if capA.seqCallID != capPBX.seqCallID {
		t.Errorf("appA call_id %q != pbx call_id %q", capA.seqCallID, capPBX.seqCallID)
	}
}

// Given multi-app chain [A,B] + PBX; When call arrives; Then every outbound leg
// carries a distinct X-Sequencer-Leg-Id (AC2).
func TestDistinctLegIDPerLeg(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "A", URI: appA.sipURI(), OnFailure: config.FailureSkip},
		{Name: "B", URI: appB.sipURI(), OnFailure: config.FailureSkip},
	}), 0)
	ctx := context.Background()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	capA := captureInvite(t, appA, []byte(testSDP2))
	capB := captureInvite(t, appB, []byte(testSDP2))
	capPBX := captureInvite(t, pbx, []byte(testSDP2))

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	if capA.seqLegID == "" || capB.seqLegID == "" || capPBX.seqLegID == "" {
		t.Fatalf("missing X-Sequencer-Leg-Id: A=%q B=%q pbx=%q",
			capA.seqLegID, capB.seqLegID, capPBX.seqLegID)
	}
	if capA.seqLegID == capB.seqLegID {
		t.Errorf("appA and appB share leg_id %q", capA.seqLegID)
	}
	if capA.seqLegID == capPBX.seqLegID {
		t.Errorf("appA and pbx share leg_id %q", capA.seqLegID)
	}
	if capB.seqLegID == capPBX.seqLegID {
		t.Errorf("appB and pbx share leg_id %q", capB.seqLegID)
	}
}

// Given single app + PBX; When two separate calls arrive; Then each call carries a
// different X-Sequencer-Call-Id (AC3).
func TestDifferentCallsDifferentCallID(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	autoAnswer(t, pbx, "", nil)

	// Call 1.
	caller1 := newFakeUAC(t)
	sess1, err := caller1.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller1 invite: %v", err)
	}
	cap1 := captureInvite(t, app, []byte(testSDP2))
	if err := sess1.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller1 WaitAnswer: %v", err)
	}
	if err := sess1.Ack(ctx); err != nil {
		t.Fatalf("caller1 ACK: %v", err)
	}

	// Call 2.
	caller2 := newFakeUAC(t)
	sess2, err := caller2.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller2 invite: %v", err)
	}
	cap2 := captureInvite(t, app, []byte(testSDP2))
	if err := sess2.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller2 WaitAnswer: %v", err)
	}
	if err := sess2.Ack(ctx); err != nil {
		t.Fatalf("caller2 ACK: %v", err)
	}

	if cap1.seqCallID == "" || cap2.seqCallID == "" {
		t.Fatalf("call IDs must be non-empty: call1=%q call2=%q", cap1.seqCallID, cap2.seqCallID)
	}
	if cap1.seqCallID == cap2.seqCallID {
		t.Fatalf("two calls share X-Sequencer-Call-Id=%q", cap1.seqCallID)
	}
}

// Given sequence [tap-app, none-app] + PBX; When call arrives; Then every outbound
// INVITE (incl. tap leg and PBX) carries both X-Sequencer-Call-Id and
// X-Sequencer-Leg-Id, and all share the same call_id (AC4).
func TestHeadersOnEveryOutboundHopInclPBX(t *testing.T) {
	tapApp := newFakeUAS(t)
	noneApp := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	s1, s2 := newTapReceivers(t)

	listenAddr := freeAddr(t)
	cfg := config.Config{
		SIP:     config.SIP{Listen: listenAddr},
		NextHop: config.NextHop{URI: pbx.sipURI()},
		RTP:     config.RTP{PortRange: "20000-20100"},
		Sequence: []config.Application{
			{Name: "tapapp", URI: tapApp.sipURI(), OnFailure: config.FailureSkip, Media: config.MediaTap},
			{Name: "noneapp", URI: noneApp.sipURI(), OnFailure: config.FailureSkip},
		},
	}
	startEngine(t, cfg, 0)
	ctx := context.Background()

	tapSDPBytes := tapAnswerSDP(
		addrHost(s1.LocalAddr()),
		addrPort(t, s1.LocalAddr()),
		addrPort(t, s2.LocalAddr()),
	)

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	capTap := captureInvite(t, tapApp, tapSDPBytes)
	capNone := captureInvite(t, noneApp, []byte(testSDP2))
	capPBX := captureInvite(t, pbx, []byte(testSDP2))

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	for _, tc := range []struct {
		name      string
		seqCallID string
		seqLegID  string
	}{
		{"tap", capTap.seqCallID, capTap.seqLegID},
		{"none", capNone.seqCallID, capNone.seqLegID},
		{"pbx", capPBX.seqCallID, capPBX.seqLegID},
	} {
		if tc.seqCallID == "" {
			t.Errorf("%s leg missing X-Sequencer-Call-Id", tc.name)
		}
		if tc.seqLegID == "" {
			t.Errorf("%s leg missing X-Sequencer-Leg-Id", tc.name)
		}
	}
	if capTap.seqCallID != capNone.seqCallID || capNone.seqCallID != capPBX.seqCallID {
		t.Errorf("X-Sequencer-Call-Id differs across legs: tap=%q none=%q pbx=%q",
			capTap.seqCallID, capNone.seqCallID, capPBX.seqCallID)
	}
}

// Given single app + PBX; When call established; Then SIP Call-IDs per leg remain
// distinct (sipgo-managed correlation unaffected by sequencer headers) (AC5).
func TestInternalSIPMappingUnaffected(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	capApp := captureInvite(t, app, []byte(testSDP2))
	capPBX := captureInvite(t, pbx, []byte(testSDP2))

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("ACK: %v", err)
	}

	inboundSIPCallID := sess.InviteRequest.CallID().Value()

	// SIP Call-IDs must remain distinct per leg (sipgo manages them independently).
	if inboundSIPCallID == capApp.sipCallID {
		t.Errorf("inbound and app leg share SIP Call-ID %q", inboundSIPCallID)
	}
	if inboundSIPCallID == capPBX.sipCallID {
		t.Errorf("inbound and pbx leg share SIP Call-ID %q", inboundSIPCallID)
	}
	if capApp.sipCallID == capPBX.sipCallID {
		t.Errorf("app and pbx legs share SIP Call-ID %q", capApp.sipCallID)
	}

	// Sequencer headers are present alongside sipgo's headers.
	if capApp.seqCallID == "" {
		t.Error("app leg missing X-Sequencer-Call-Id")
	}
	if capPBX.seqCallID == "" {
		t.Error("pbx leg missing X-Sequencer-Call-Id")
	}
	if capApp.seqCallID != capPBX.seqCallID {
		t.Errorf("app and pbx carry different X-Sequencer-Call-Id: %q vs %q",
			capApp.seqCallID, capPBX.seqCallID)
	}
}
