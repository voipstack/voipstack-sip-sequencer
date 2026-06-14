//go:build e2e

package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emiago/sipgo"
)

// NOTE: media relay from a WebSocket-originated call is intentionally NOT covered
// here. The engine forwards RTP only after the inbound dialog is ACK-confirmed
// (verified: a UDP caller that skips the ACK gets no media; the same caller with an
// ACK does). A WS/WSS caller cannot ACK because the engine's dialog Contact is the
// UDP signaling address regardless of inbound transport (no per-transport Contact
// rewrite — see connectTo). So media-from-WS is unreachable black-box until that
// product gap is closed. Signaling relay to apps from WS is covered below.

// Given a WS caller and a multi-app sequence; When the call arrives over WebSocket;
// Then the engine relays it to each SIP application (over TCP) in configured order
// before the PBX, and the caller reaches 200 OK.
func TestWebSocketCallRelaysToAppChain(t *testing.T) {
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
	cfg.WSListen = freeTCPPort(t)
	startReady(t, cfg)
	waitTCPListening(t, cfg.WSListen, 5*time.Second)

	connectTo(t, caller, "sip:"+cfg.WSListen+";transport=ws")

	if got, want := log.snapshot(), []string{"A", "B", "C"}; !equalOrder(got, want) {
		t.Fatalf("app visit order for WS call = %v, want %v", got, want)
	}
}

// Given a WS caller; When the call is relayed to an application; Then that app's
// INVITE carries the sequencer correlation headers, exactly as for a UDP caller.
func TestWebSocketCallAppReceivesHeaders(t *testing.T) {
	appA := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	log := newChainLog()
	serveApp(t, appA, "A", log, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appA, "none", "skip")})
	cfg.WSListen = freeTCPPort(t)
	startReady(t, cfg)
	waitTCPListening(t, cfg.WSListen, 5*time.Second)

	connectTo(t, caller, "sip:"+cfg.WSListen+";transport=ws")

	reqA := log.request("A")
	if reqA == nil {
		t.Fatal("app A never received an INVITE from the WS call")
	}
	if headerValue(t, reqA, "X-Sequencer-Call-Id") == "" {
		t.Fatal("relayed app INVITE missing X-Sequencer-Call-Id")
	}
	if headerValue(t, reqA, "X-Sequencer-Leg-Id") == "" {
		t.Fatal("relayed app INVITE missing X-Sequencer-Leg-Id")
	}
}

// Given a WS caller and an app with on_failure: abort that rejects; When the call
// arrives over WebSocket; Then the rejection is propagated back to the WS caller.
func TestWebSocketCallAppAbortPropagates(t *testing.T) {
	appA := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	serveApp(t, appA, "A", nil, 486, "Busy Here", nil)

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appA, "none", "abort")})
	cfg.WSListen = freeTCPPort(t)
	startReady(t, cfg)
	waitTCPListening(t, cfg.WSListen, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := caller.invite(ctx, "sip:"+cfg.WSListen+";transport=ws", []byte(testSDP))
	if err != nil {
		t.Fatalf("WS caller invite: %v", err)
	}
	err = sess.WaitAnswer(ctx, sipgo.AnswerOptions{})
	var dialErr *sipgo.ErrDialogResponse
	if !errors.As(err, &dialErr) {
		t.Fatalf("expected ErrDialogResponse over WS, got %v", err)
	}
	if dialErr.Res.StatusCode != 486 {
		t.Fatalf("expected 486 propagated to WS caller, got %d", dialErr.Res.StatusCode)
	}
	pbx.noInvite(t, 300*time.Millisecond)
}

// Given a WSS caller and an app sequence; When the secure call arrives; Then it is
// relayed to the application chain and reaches 200 OK end to end.
func TestSecureWebSocketCallRelaysToApp(t *testing.T) {
	appA := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newSecureWSCaller(t)

	log := newChainLog()
	serveApp(t, appA, "A", log, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appA, "none", "abort")})
	cfg.WSSListen = freeTCPPort(t)
	cfg.WSSProfile = "wsslistener"
	cfg.TLSProfiles = map[string]yamlTLSProfile{"wsslistener": listenerProfile(t)}
	startReady(t, cfg)
	waitTCPListening(t, cfg.WSSListen, 5*time.Second)

	connectTo(t, caller, "sip:"+cfg.WSSListen+";transport=wss")

	if log.request("A") == nil {
		t.Fatal("app A never received the relayed INVITE from the WSS call")
	}
}
