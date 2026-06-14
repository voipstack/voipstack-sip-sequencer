//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// PRD §5: only call methods are managed; every other method is transparently
// proxied to the terminating next-hop and never enters the application chain.
func TestUnmanagedMethodsProxiedToNextHop(t *testing.T) {
	nh := newFakeNextHop(t)
	appSrv := newFakeUAS(t)
	caller := newSimpleCaller(t)

	serveApp(t, appSrv, "A", nil, 200, "OK", []byte(testSDP2))

	cfg := baseConfigURI(t, nh.sipURI(), []yamlApp{app("A", appSrv, "none", "skip")})
	s := startReady(t, cfg)

	for _, m := range []sip.RequestMethod{sip.REGISTER, sip.OPTIONS} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		res, err := caller.send(ctx, m, "sip:"+s.sipListen)
		cancel()
		if err != nil {
			t.Fatalf("send %s: %v", m, err)
		}
		if res.StatusCode != 200 {
			t.Fatalf("%s: expected 200 from next-hop, got %d", m, res.StatusCode)
		}
		if got := nh.waitMethod(t, 2*time.Second); got != m {
			t.Fatalf("next-hop received %s, want %s", got, m)
		}
	}
	// The application never sees unmanaged methods — only calls.
	appSrv.noInvite(t, 300*time.Millisecond)
}

// PRD §4/§5: a media: none application (also the default) is offered audio
// inactive — no RTP is sent to it.
func TestMediaNoneAppOfferedInactive(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	log := newChainLog()
	serveApp(t, appSrv, "A", log, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	// media omitted -> default none.
	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "", "skip")})
	s := startReady(t, cfg)

	establish(t, caller, s.sipListen)

	offer := log.request("A").Body()
	if !bytes.Contains(offer, []byte("a=inactive")) {
		t.Fatalf("media:none app offer is not inactive:\n%s", offer)
	}
}

// PRD §5: a media: tap application is offered a recvonly two-m=audio (stereo)
// session — one stream per call direction.
func TestTapAppOfferedRecvonlyStereo(t *testing.T) {
	tapApp := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	log := newChainLog()
	serveApp(t, tapApp, "tap", log, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("tap", tapApp, "tap", "skip")})
	s := startReady(t, cfg)

	establish(t, caller, s.sipListen)

	offer := log.request("tap").Body()
	if n := bytes.Count(offer, []byte("m=audio")); n != 2 {
		t.Fatalf("tap offer has %d m=audio lines, want 2 (stereo):\n%s", n, offer)
	}
	if n := bytes.Count(offer, []byte("a=recvonly")); n != 2 {
		t.Fatalf("tap offer has %d recvonly streams, want 2:\n%s", n, offer)
	}
}

// PRD §5: no transcoding — the codec the parties negotiate is carried unchanged to
// every leg. The offer the engine sends the PBX preserves the caller's codec.
func TestCodecPreservedToPBX(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	serveApp(t, appSrv, "A", nil, 200, "OK", []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "none", "skip")})
	s := startReady(t, cfg)

	var pbxOffer []byte
	done := make(chan struct{})
	go func() {
		dss := pbx.waitInvite(t, 5*time.Second)
		pbxOffer = dss.InviteRequest.Body()
		_ = dss.Respond(200, "OK", []byte(testSDP2))
		close(done)
	}()

	// Caller advertises PCMU (rtpmap:0). sdpWithAddr embeds "a=rtpmap:0 PCMU/8000".
	epConn, epHost, epPort := udpSocket(t)
	_ = epConn
	establishWithOffer(t, caller, "sip:"+s.sipListen, sdpWithAddr(epHost, epPort))
	<-done

	if !bytes.Contains(pbxOffer, []byte("rtpmap:0 PCMU/8000")) {
		t.Fatalf("PBX offer dropped/altered the caller codec (transcoding?):\n%s", pbxOffer)
	}
}

// PRD §7: on_failure defaults to skip when omitted — a dead optional app must not
// kill calls.
func TestDefaultOnFailureIsSkip(t *testing.T) {
	appA := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	serveApp(t, appA, "A", nil, 486, "Busy Here", nil) // rejects
	autoAnswer(t, pbx, []byte(testSDP2))

	// on_failure omitted -> default skip; the rejecting app must be skipped.
	cfg := baseConfig(t, pbx, []yamlApp{app("A", appA, "none", "")})
	s := startReady(t, cfg)

	establish(t, caller, s.sipListen)
}

// PRD §9: a terminating-hop (PBX) failure is counted in
// sequencer_terminating_hop_failures_total and the call fails.
func TestTerminatingHopFailureMetric(t *testing.T) {
	appSrv := newFakeUAS(t)
	caller := newFakeUAC(t)

	serveApp(t, appSrv, "A", nil, 200, "OK", []byte(testSDP2))

	cfg := baseConfigURI(t, deadAddr(t), []yamlApp{app("A", appSrv, "none", "skip")})
	cfg.NextHopTransport = "tcp" // dead TCP port -> connection refused fast
	s := startReady(t, cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := caller.invite(ctx, "sip:"+s.sipListen, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err == nil {
		t.Fatal("expected call failure when PBX is unreachable")
	}

	_, body := mustGet(t, "http://"+s.obsListen+"/metrics")
	if !strings.Contains(body, "sequencer_terminating_hop_failures_total 1") {
		t.Fatalf("missing terminating-hop failure metric\nbody:\n%s", body)
	}
}

// PRD §9: the sequencer serves concurrent calls. A batch of simultaneous callers
// each connect end to end.
func TestConcurrentCallsConnect(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)

	autoAnswer(t, appSrv, []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "none", "skip")})
	cfg.RTPRange = freeRTPRangeSpan(t, 400) // headroom: ~2 pairs per concurrent call
	s := startReady(t, cfg)

	const n = 20
	// Build callers up front (sipgo UA setup touches *testing.T) so goroutines only
	// do socket I/O.
	callers := make([]*fakeUAC, n)
	for i := range callers {
		callers[i] = newFakeUAC(t)
	}

	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(c *fakeUAC) {
			defer wg.Done()
			errs <- placeCall(c, "sip:"+s.sipListen)
		}(callers[i])
	}
	wg.Wait()
	close(errs)

	failed := 0
	for err := range errs {
		if err != nil {
			failed++
			t.Logf("concurrent call failed: %v", err)
		}
	}
	if failed != 0 {
		t.Fatalf("%d/%d concurrent calls failed", failed, n)
	}
}

// placeCall drives one INVITE/answer/ACK and returns any error (safe to call from a
// goroutine — it does not touch *testing.T).
func placeCall(caller *fakeUAC, targetURI string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	sess, err := caller.invite(ctx, targetURI, []byte(testSDP))
	if err != nil {
		return err
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		return err
	}
	if sess.InviteResponse.StatusCode != 200 {
		return &statusError{sess.InviteResponse.StatusCode}
	}
	return sess.Ack(ctx)
}

type statusError struct{ code int }

func (e *statusError) Error() string { return "non-200 answer: " + strconv.Itoa(e.code) }

// PRD §6: a missing required key fails fast with a clear error.
func TestMissingRequiredKeyFailsFast(t *testing.T) {
	dir := t.TempDir()
	// Valid YAML but next_hop omitted (required).
	raw := "" +
		"sip:\n  listen: \"127.0.0.1:5060\"\n" +
		"rtp:\n  port_range: \"10000-10010\"\n" +
		"sequence:\n  - name: app1\n    uri: \"sip:127.0.0.1:5080\"\n"
	cfgPath := writeRawConfig(t, dir, raw)

	s := start(t, cfgPath, "", "")
	if err := s.waitExit(t, 5*time.Second); err == nil {
		t.Fatalf("expected non-zero exit for missing next_hop\nstderr:\n%s", s.stderr.String())
	}
	if !strings.Contains(s.stderr.String(), "error:") {
		t.Fatalf("stderr missing validation error\nstderr:\n%s", s.stderr.String())
	}
}

// PRD §5: a mid-call re-INVITE propagates through the existing legs; the chain is
// NOT re-run (the application receives no second INVITE).
func TestReInviteChainNotReRun(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	// The app answers exactly one INVITE; a re-run would deliver a second.
	go func() {
		dss := appSrv.waitInvite(t, 5*time.Second)
		_ = dss.Respond(200, "OK", []byte(testSDP2))
	}()
	autoAnswer(t, pbx, []byte(testSDP2)) // answers the initial INVITE and the re-INVITE

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "none", "skip")})
	s := startReady(t, cfg)

	sess := establishWithOffer(t, caller, "sip:"+s.sipListen, sdpWithAddr("127.0.0.1", 40000))

	// Caller re-INVITEs with a new media port, routed to the engine's Contact.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reInvite := sip.NewRequest(sip.INVITE, sess.InviteResponse.Contact().Address)
	reInvite.SetBody(sdpWithAddr("127.0.0.1", 40002))
	reInvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	res, err := sess.Do(ctx, reInvite)
	if err != nil {
		t.Fatalf("caller re-INVITE: %v", err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("re-INVITE answer: expected 200, got %d", res.StatusCode)
	}

	// Chain not re-run: the app gets no second INVITE.
	appSrv.noInvite(t, 400*time.Millisecond)
}
