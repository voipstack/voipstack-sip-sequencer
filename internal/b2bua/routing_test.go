package b2bua

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// noInviteWait is the window routing tests wait to be sure a guarded app was
// skipped — long enough to be stable, short enough to keep the suite fast.
const noInviteWait = 200 * time.Millisecond

func regexRule(from, to, method string) *config.ResolvedRouting {
	r := &config.ResolvedRouting{Method: method}
	if from != "" {
		r.FromRe = regexp.MustCompile(from)
	}
	if to != "" {
		r.ToRe = regexp.MustCompile(to)
	}
	return r
}

// Given two apps where the first's routing rule rejects the caller's From;
// When the call is placed; Then app1 receives no INVITE, app2 (no rule) does,
// and the call still completes through to the PBX.
func TestRoutingSkipsAppWhoseRuleDoesNotMatch(t *testing.T) {
	appSkip := newFakeUAS(t)
	appKeep := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	cfg := multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{
			Name: "guarded", URI: appSkip.sipURI(), OnFailure: config.FailureSkip,
			RoutingRe: regexRule(`^sip:bob@`, "", "INVITE"),
		},
		{
			Name: "open", URI: appKeep.sipURI(), OnFailure: config.FailureSkip,
		},
	})
	startEngine(t, cfg, 0)
	ctx := context.Background()

	from, to := callerIdentity()
	sess, err := inviteWithHeaders(ctx, caller, "sip:"+listenAddr, []byte(testSDP), from, to)
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	appSkip.noInvite(t, noInviteWait)
	keepReq := captureInviteReq(t, appKeep, []byte(testSDP2))
	_ = captureInviteReq(t, pbx, []byte(testSDP2))

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	if got := keepReq.From(); got == nil || got.Address.User != "alice" {
		t.Fatalf("keep app leg: From not preserved: %v", got)
	}
}

// Given an app whose routing rule matches the caller's From; When the call is
// placed; Then that app receives the INVITE.
func TestRoutingAdmitsAppWhoseRuleMatches(t *testing.T) {
	appMatch := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	cfg := multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{
			Name: "guarded", URI: appMatch.sipURI(), OnFailure: config.FailureSkip,
			RoutingRe: regexRule(`^sip:alice@`, "", "INVITE"),
		},
	})
	startEngine(t, cfg, 0)
	ctx := context.Background()

	from, to := callerIdentity()
	sess, err := inviteWithHeaders(ctx, caller, "sip:"+listenAddr, []byte(testSDP), from, to)
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	matchReq := captureInviteReq(t, appMatch, []byte(testSDP2))
	_ = captureInviteReq(t, pbx, []byte(testSDP2))

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	if got := matchReq.From(); got == nil || got.Address.User != "alice" {
		t.Fatalf("matching app leg: From not preserved: %v", got)
	}
}

// Given an app whose routing rule requires method OPTIONS; When an INVITE is
// placed; Then the app is skipped (method mismatch) and the call proceeds to PBX.
func TestRoutingSkipsAppOnMethodMismatch(t *testing.T) {
	appSkip := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	cfg := multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{
			Name: "options-only", URI: appSkip.sipURI(), OnFailure: config.FailureSkip,
			RoutingRe: regexRule("", "", "OPTIONS"),
		},
	})
	startEngine(t, cfg, 0)
	ctx := context.Background()

	from, to := callerIdentity()
	sess, err := inviteWithHeaders(ctx, caller, "sip:"+listenAddr, []byte(testSDP), from, to)
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	appSkip.noInvite(t, noInviteWait)
	_ = captureInviteReq(t, pbx, []byte(testSDP2))

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}
}

// Given two apps, both guarded by a from-rule, where only the first matches;
// When the call is placed; Then only the first app and the PBX receive INVITEs.
func TestRoutingSkipsSecondAppWhenOnlyFirstMatches(t *testing.T) {
	appA := newFakeUAS(t)
	appB := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	cfg := multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{
			Name: "alice-only", URI: appA.sipURI(), OnFailure: config.FailureSkip,
			RoutingRe: regexRule(`^sip:alice@`, "", ""),
		},
		{
			Name: "carol-only", URI: appB.sipURI(), OnFailure: config.FailureSkip,
			RoutingRe: regexRule(`^sip:carol@`, "", ""),
		},
	})
	startEngine(t, cfg, 0)
	ctx := context.Background()

	from, to := callerIdentity() // alice → bob
	sess, err := inviteWithHeaders(ctx, caller, "sip:"+listenAddr, []byte(testSDP), from, to)
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	_ = captureInviteReq(t, appA, []byte(testSDP2))
	appB.noInvite(t, noInviteWait)
	_ = captureInviteReq(t, pbx, []byte(testSDP2))

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}
}

// Compile-time guard: ensure sip is referenced for the headers used in helpers.
var _ = sip.NewHeader
