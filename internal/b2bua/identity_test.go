package b2bua

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// ── identity-transparency tests ───────────────────────────────────────────────
//
// Every outbound leg (app + PBX) must look like the original inbound INVITE to its
// target: the caller's From/To and pass-through identity headers are carried
// verbatim, while the sequencer owns Contact/Via/Call-ID/CSeq/Max-Forwards.

// inviteWithHeaders sends an INVITE from the caller fake carrying caller-supplied
// identity/pass-through headers so identity transparency can be asserted.
func inviteWithHeaders(ctx context.Context, f *fakeUAC, targetURI string, sdp []byte, headers ...sip.Header) (*sipgo.DialogClientSession, error) {
	var uri sip.Uri
	if err := sip.ParseUri(targetURI, &uri); err != nil {
		return nil, err
	}
	return f.dcc.Invite(ctx, uri, sdp, headers...)
}

// callerIdentity builds a distinctive caller From/To so outbound legs can be checked.
func callerIdentity() (from *sip.FromHeader, to *sip.ToHeader) {
	from = &sip.FromHeader{
		DisplayName: "Alice",
		Address:     sip.Uri{Scheme: "sip", User: "alice", Host: "caller.example.com"},
		Params:      sip.NewParams(),
	}
	from.Params.Add("tag", "caller-tag-001")
	to = &sip.ToHeader{
		Address: sip.Uri{Scheme: "sip", User: "bob", Host: "pbx.example.com"},
		Params:  sip.NewParams(),
	}
	return from, to
}

// captureInviteReq blocks until uas receives one INVITE, returns the full request,
// and answers 200 with sdp. Bridge originates legs in order (app→PBX), so call this
// in that order from the test goroutine. Must be called before the caller's
// WaitAnswer, since the 200 to the caller only arrives after the PBX leg answers.
func captureInviteReq(t *testing.T, uas *fakeUAS, sdp []byte) *sip.Request {
	t.Helper()
	select {
	case dss := <-uas.invites:
		req := dss.InviteRequest
		_ = dss.Respond(200, "OK", sdp)
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("captureInviteReq: timeout waiting for INVITE")
		return nil
	}
}

// assertCallerIdentity verifies the caller's From/To survived verbatim onto the leg
// and that Contact is the sequencer's (not the caller's domain).
func assertCallerIdentity(t *testing.T, req *sip.Request, leg string) {
	t.Helper()
	from := req.From()
	if from == nil {
		t.Fatalf("%s leg: no From header", leg)
	}
	if from.Address.User != "alice" || from.Address.Host != "caller.example.com" {
		t.Fatalf("%s leg: From not preserved: got %s@%s, want alice@caller.example.com",
			leg, from.Address.User, from.Address.Host)
	}
	if from.DisplayName != "Alice" {
		t.Fatalf("%s leg: From display name not preserved: got %q", leg, from.DisplayName)
	}
	to := req.To()
	if to == nil || to.Address.User != "bob" {
		t.Fatalf("%s leg: To not preserved: got %v, want user bob", leg, to)
	}
	contact := req.Contact()
	if contact == nil {
		t.Fatalf("%s leg: missing sequencer Contact", leg)
	}
	if contact.Address.Host == "caller.example.com" {
		t.Fatalf("%s leg: Contact must be sequencer-owned, got caller's %s", leg, contact.Address.Host)
	}
}

// Given a caller with a distinctive From/To; When the call reaches the PBX;
// Then the PBX leg carries the caller's From/To verbatim, Contact is the sequencer's.
func TestPbxLegPreservesInboundFromTo(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	from, to := callerIdentity()
	sess, err := inviteWithHeaders(ctx, caller, "sip:"+listenAddr, []byte(testSDP), from, to)
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	_ = captureInviteReq(t, app, []byte(testSDP2)) // app leg first
	pbxReq := captureInviteReq(t, pbx, []byte(testSDP2))

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	assertCallerIdentity(t, pbxReq, "pbx")
}

// Given a caller with a distinctive From/To; When the call reaches the application;
// Then the app leg carries the caller's From/To verbatim, Contact is the sequencer's.
func TestAppLegPreservesInboundFromTo(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	from, to := callerIdentity()
	sess, err := inviteWithHeaders(ctx, caller, "sip:"+listenAddr, []byte(testSDP), from, to)
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	appReq := captureInviteReq(t, app, []byte(testSDP2))
	_ = captureInviteReq(t, pbx, []byte(testSDP2))

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	assertCallerIdentity(t, appReq, "app")
}

// Given a caller sending a pass-through identity header (P-Asserted-Identity);
// When the call traverses app and PBX; Then the header is carried verbatim on both legs.
func TestPassThroughHeaderPreservedOnOutboundLegs(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	from, to := callerIdentity()
	const pai = "<sip:alice@trusted.example.com>"
	sess, err := inviteWithHeaders(ctx, caller, "sip:"+listenAddr, []byte(testSDP),
		from, to, sip.NewHeader("P-Asserted-Identity", pai))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	appReq := captureInviteReq(t, app, []byte(testSDP2))
	pbxReq := captureInviteReq(t, pbx, []byte(testSDP2))

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	for leg, req := range map[string]*sip.Request{"app": appReq, "pbx": pbxReq} {
		h := req.GetHeader("P-Asserted-Identity")
		if h == nil {
			t.Fatalf("%s leg: P-Asserted-Identity not carried through", leg)
		}
		if h.Value() != pai {
			t.Fatalf("%s leg: P-Asserted-Identity mangled: got %q, want %q", leg, h.Value(), pai)
		}
	}
}

// Given a caller sending Authorization + a custom header; When the call traverses
// app and PBX; Then both arbitrary headers are relayed verbatim onto every leg.
func TestArbitraryAndAuthHeadersForwardedToOutboundLegs(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	from, to := callerIdentity()
	const auth = `Digest username="alice", realm="sip.example.com", nonce="abc123", response="deadbeef"`
	const custom = "tenant-42"
	sess, err := inviteWithHeaders(ctx, caller, "sip:"+listenAddr, []byte(testSDP),
		from, to,
		sip.NewHeader("Authorization", auth),
		sip.NewHeader("X-Tenant-Id", custom))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	appReq := captureInviteReq(t, app, []byte(testSDP2))
	pbxReq := captureInviteReq(t, pbx, []byte(testSDP2))

	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	for leg, req := range map[string]*sip.Request{"app": appReq, "pbx": pbxReq} {
		if h := req.GetHeader("Authorization"); h == nil || h.Value() != auth {
			t.Fatalf("%s leg: Authorization not relayed: got %v, want %q", leg, h, auth)
		}
		if h := req.GetHeader("X-Tenant-Id"); h == nil || h.Value() != custom {
			t.Fatalf("%s leg: custom header not relayed: got %v, want %q", leg, h, custom)
		}
		// Sequencer-owned correlation header must be the sequencer's, not spoofable.
		if h := req.GetHeader("X-Sequencer-Call-Id"); h == nil {
			t.Fatalf("%s leg: missing sequencer correlation header", leg)
		}
	}
}

// Given the PBX rejects with 401 + a challenge and a custom header; When the call
// fails; Then the caller's final response carries the challenge and custom header
// verbatim (so the caller can authenticate through the B2BUA).
func TestAuthChallengeAndHeadersRelayedToCaller(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	autoAnswer(t, app, "", nil) // app answers 2xx; failure comes from the PBX leg

	const challenge = `Digest realm="sip.example.com", nonce="xyz789"`
	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		_ = dss.Respond(401, "Unauthorized", nil,
			sip.NewHeader("WWW-Authenticate", challenge),
			sip.NewHeader("X-Reject-Note", "needs-auth"))
	}()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}

	err = sess.WaitAnswer(ctx, sipgo.AnswerOptions{})
	var dialErr *sipgo.ErrDialogResponse
	if !errors.As(err, &dialErr) {
		t.Fatalf("expected dialog response error, got %v", err)
	}
	res := dialErr.Res
	if res.StatusCode != 401 {
		t.Fatalf("expected 401 relayed, got %d", res.StatusCode)
	}
	if h := res.GetHeader("WWW-Authenticate"); h == nil || h.Value() != challenge {
		t.Fatalf("WWW-Authenticate challenge lost: got %v, want %q", h, challenge)
	}
	if h := res.GetHeader("X-Reject-Note"); h == nil || h.Value() != "needs-auth" {
		t.Fatalf("custom reject header lost: got %v", h)
	}
}

// Given the PBX answers 200 with a custom header; When the call establishes;
// Then the caller's 200 OK carries that header verbatim.
func TestPbxSuccessHeaderRelayedToCaller(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	startEngine(t, testConfig(listenAddr, app.sipURI(), pbx.sipURI()), 0)
	ctx := context.Background()

	autoAnswer(t, app, "", nil)

	go func() {
		dss := pbx.waitInvite(t, 3*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2),
			sip.NewHeader("X-Pbx-Note", "session-7"))
	}()

	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}

	if h := sess.InviteResponse.GetHeader("X-Pbx-Note"); h == nil || h.Value() != "session-7" {
		t.Fatalf("PBX 200 custom header lost: got %v", h)
	}
}
