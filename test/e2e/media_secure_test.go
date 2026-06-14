//go:build e2e

package e2e

import (
	"testing"
	"time"
)

// RTP over a secure / WebSocket inbound transport (TLS, WS, WSS) is NOT covered by
// a media-flow test, and cannot be black-box: the engine forwards RTP only after the
// inbound dialog is ACK-confirmed (proven in ws_apps_test.go — a UDP caller without
// an ACK gets no media), and a non-UDP caller cannot ACK. The dialog Contact the
// engine returns is always the UDP signaling address (no per-transport rewrite), so
// an in-dialog ACK from a TLS/WS/WSS caller dials that UDP port over the secure
// transport and is refused. Verified directly for both transports:
//   - WS:  ACK -> "transport<WS> dial 127.0.0.1:<udp>: connection refused"
//   - TLS: ACK -> "dial TCP error: dial 127.0.0.1:<udp>: connection refused"
// RTP transmission is therefore covered over UDP only (media_test.go), which is the
// transport whose caller can complete the dialog. Closing the Contact-rewrite gap in
// the product would unblock media tests for every secure transport.
//
// What we CAN verify black-box for a secure inbound transport is signaling: the
// listener accepts the call and the chain reaches a 200 OK.

// Given a SIP-over-TLS inbound listener; When a TLS caller places a call; Then it is
// bridged through the app chain and reaches 200 OK end to end.
func TestTLSInboundCallerConnects(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newSecureCaller(t)

	serveApp(t, appSrv, "A", nil, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "none", "abort")})
	cfg.TLSListen = freeTCPPort(t)
	cfg.TLSProfile = "inbound"
	cfg.TLSProfiles = map[string]yamlTLSProfile{"inbound": listenerProfile(t)}
	startReady(t, cfg)
	waitTCPListening(t, cfg.TLSListen, 5*time.Second)

	connectTo(t, caller, "sip:"+cfg.TLSListen+";transport=tls")
}
