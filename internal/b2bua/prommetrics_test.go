package b2bua

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// ── observability test helpers ───────────────────────────────────────────────

// freeTCPAddr grabs a TCP port and releases it for the obs server to bind.
// Mirrors freeAddr; the same small TOCTOU race is acceptable in tests.
func freeTCPAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeTCPAddr: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// obsConfig is testConfig plus an observability listen address.
func obsConfig(listenAddr, appURI, pbxURI, obsAddr string) config.Config {
	cfg := testConfig(listenAddr, appURI, pbxURI)
	cfg.Observability.Listen = obsAddr
	return cfg
}

// startObsEngine starts an engine wired with a real PromMetrics sink and an
// observability HTTP server, faking only the external scraper/prober (real GETs).
func startObsEngine(t *testing.T, cfg config.Config) *Engine {
	t.Helper()
	return startEngine(t, cfg, 0, NewPromMetrics())
}

// scrape issues a real HTTP GET against the in-process server and returns the status
// and body. A transient request error (the obs HTTP server binds on its own goroutine,
// so an early scrape can race the bind) returns 0,"" so the caller's retry loop can
// poll until the server is up rather than failing the test on the first refusal.
func scrape(t *testing.T, obsAddr, path string) (int, string) {
	t.Helper()
	resp, err := http.Get("http://" + obsAddr + path)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, ""
	}
	return resp.StatusCode, string(body)
}

// waitMetric polls /metrics until it contains want or the deadline elapses. Used
// because counter emission and teardown happen across goroutines.
func waitMetric(t *testing.T, obsAddr, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		_, last = scrape(t, obsAddr, "/metrics")
		if strings.Contains(last, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("metric %q not found within %s; last scrape:\n%s", want, timeout, last)
}

// establishObsCall drives a full caller→engine→app→PBX setup and returns the caller
// session (so the test can BYE it). app and pbx must already be auto-answering.
func establishObsCall(t *testing.T, caller *fakeUAC, listenAddr string) *sipgo.DialogClientSession {
	t.Helper()
	ctx := context.Background()
	sess, err := caller.invite(ctx, "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	if err := sess.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("caller WaitAnswer: %v", err)
	}
	if sess.InviteResponse.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", sess.InviteResponse.StatusCode)
	}
	if err := sess.Ack(ctx); err != nil {
		t.Fatalf("caller ACK: %v", err)
	}
	return sess
}

// ── behavior tests ────────────────────────────────────────────────────────────

// Given live calls; When /metrics is scraped; Then sequencer_active_calls equals
// the live count and drops to 0 after teardown (AC1).
func TestActiveCallsGaugeReflectsLiveCalls(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller1 := newFakeUAC(t)
	caller2 := newFakeUAC(t)

	listenAddr := freeAddr(t)
	obsAddr := freeTCPAddr(t)
	eng := startObsEngine(t, obsConfig(listenAddr, app.sipURI(), pbx.sipURI(), obsAddr))

	autoAnswer(t, app, "", nil)
	autoAnswer(t, pbx, "", nil)

	sess1 := establishObsCall(t, caller1, listenAddr)
	_ = establishObsCall(t, caller2, listenAddr)

	if n := eng.calls.ActiveCalls(); n != 2 {
		t.Fatalf("ActiveCalls() = %d, want 2", n)
	}
	waitMetric(t, obsAddr, "sequencer_active_calls 2", 3*time.Second)

	// End one call; gauge must reflect the live map without manual dec.
	if err := sess1.Bye(context.Background()); err != nil {
		t.Fatalf("caller1 BYE: %v", err)
	}
	waitMetric(t, obsAddr, "sequencer_active_calls 1", 3*time.Second)
}

// Given a successful app leg; When /metrics is scraped; Then
// sequencer_app_invocations_total{app="testapp"} is 1 (AC2).
func TestAppInvocationCountedPerSuccessfulLeg(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	obsAddr := freeTCPAddr(t)
	startObsEngine(t, obsConfig(listenAddr, app.sipURI(), pbx.sipURI(), obsAddr))

	autoAnswer(t, app, "", nil)
	autoAnswer(t, pbx, "", nil)

	establishObsCall(t, caller, listenAddr)

	waitMetric(t, obsAddr, `sequencer_app_invocations_total{app="testapp"} 1`, 3*time.Second)
}

// Given an app that fails under skip policy; When /metrics is scraped; Then
// sequencer_app_failures_total{app="appA"} is 1 (AC3).
func TestAppFailureCountedPerFailure(t *testing.T) {
	appA := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	obsAddr := freeTCPAddr(t)
	cfg := multiAppConfig(listenAddr, pbx.sipURI(), []config.Application{
		{Name: "appA", URI: appA.sipURI(), OnFailure: config.FailureSkip},
	})
	cfg.Observability.Listen = obsAddr
	startObsEngine(t, cfg)

	autoReject(t, appA, 503, "Service Unavailable")
	autoAnswer(t, pbx, "", nil)

	establishObsCall(t, caller, listenAddr)

	waitMetric(t, obsAddr, `sequencer_app_failures_total{app="appA"} 1`, 3*time.Second)
}

// Given a PBX that rejects the terminating hop; When /metrics is scraped; Then
// sequencer_terminating_hop_failures_total is 1, distinct from app failures (AC4).
func TestTerminatingHopFailureCounted(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	obsAddr := freeTCPAddr(t)
	startObsEngine(t, obsConfig(listenAddr, app.sipURI(), pbx.sipURI(), obsAddr))

	autoAnswer(t, app, "", nil)
	autoReject(t, pbx, 486, "Busy Here")

	// Call fails at the terminating hop; caller gets a non-200.
	sess, err := caller.invite(context.Background(), "sip:"+listenAddr, []byte(testSDP))
	if err != nil {
		t.Fatalf("caller invite: %v", err)
	}
	_ = sess.WaitAnswer(context.Background(), sipgo.AnswerOptions{})

	waitMetric(t, obsAddr, "sequencer_terminating_hop_failures_total 1", 3*time.Second)
}

// Given an established call; When /metrics is scraped; Then the sequencing
// histogram has recorded one observation (AC5).
func TestSequencingLatencyObserved(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	listenAddr := freeAddr(t)
	obsAddr := freeTCPAddr(t)
	startObsEngine(t, obsConfig(listenAddr, app.sipURI(), pbx.sipURI(), obsAddr))

	autoAnswer(t, app, "", nil)
	autoAnswer(t, pbx, "", nil)

	establishObsCall(t, caller, listenAddr)

	waitMetric(t, obsAddr, "sequencer_sequencing_duration_seconds_count 1", 3*time.Second)
}

// Given a running process; When /health is probed; Then it returns 200 "ok" (AC6).
func TestHealthEndpointReportsLiveness(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)

	listenAddr := freeAddr(t)
	obsAddr := freeTCPAddr(t)
	startObsEngine(t, obsConfig(listenAddr, app.sipURI(), pbx.sipURI(), obsAddr))

	var status int
	var body string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, body = scrape(t, obsAddr, "/health")
		if status == http.StatusOK {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status != http.StatusOK {
		t.Fatalf("/health status = %d, want 200", status)
	}
	if body != "ok" {
		t.Fatalf("/health body = %q, want %q", body, "ok")
	}
}
