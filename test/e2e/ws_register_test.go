//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

// viaValues returns the Value() of every Via header on a message, in order.
func viaValues(msg interface{ GetHeaders(string) []sip.Header }) []string {
	hs := msg.GetHeaders("Via")
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Value()
	}
	return out
}

// Given a SIP-over-WebSocket client REGISTERs through the sequencer; When the
// sequencer proxies the REGISTER to the next hop; Then the next hop receives
// exactly one proxy Via on top plus the client's single Via (no duplication), and
// the relayed response carries only the client's Via. A strict WS client (jsSIP)
// drops a response with more than one Via, so a duplicated Via list breaks digest
// auth and the registration never completes.
func TestWebSocketRegisterForwardsSingleViaSet(t *testing.T) {
	nh := newFakeNextHop(t)
	appSrv := newFakeUAS(t)
	caller := newSimpleCaller(t)

	serveApp(t, appSrv, "A", nil, 200, "OK", []byte(testSDP2))

	cfg := baseConfigURI(t, nh.sipURI(), []yamlApp{app("A", appSrv, "none", "skip")})
	cfg.WSListen = freeTCPPort(t)
	startReady(t, cfg)
	waitTCPListening(t, cfg.WSListen, 5*time.Second)

	// When: a WS client REGISTERs through the sequencer.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := caller.send(ctx, sip.REGISTER, "sip:"+cfg.WSListen+";transport=ws")
	if err != nil {
		t.Fatalf("ws REGISTER: %v", err)
	}

	// Then: the next hop sees exactly two Via headers — the proxy's, then the client's.
	req := nh.waitRequest(t, 2*time.Second)
	fwdVias := viaValues(req)
	t.Logf("next-hop saw %d Via headers: %#v", len(fwdVias), fwdVias)
	if len(fwdVias) != 2 {
		t.Fatalf("next-hop Via count = %d, want 2 (proxy + client); duplicated Via list: %#v", len(fwdVias), fwdVias)
	}
	if !strings.Contains(fwdVias[1], "WS") {
		t.Fatalf("expected the client (WS) Via second, got %#v", fwdVias)
	}

	// Then: the response relayed back to the WS client carries only the client's Via.
	respVias := viaValues(res)
	t.Logf("ws client saw %d response Via headers: %#v", len(respVias), respVias)
	if len(respVias) != 1 {
		t.Fatalf("response Via count = %d, want 1 (client only): %#v", len(respVias), respVias)
	}
}
