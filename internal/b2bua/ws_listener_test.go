package b2bua

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/gobwas/ws"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
	"github.com/voipstack/voipstack-sip-sequencer/internal/tlsprov"
)

// ── config + engine-starter helpers (reuse the TLS cert helpers in this package) ─

// wsConfig builds a config with a plain UDP listener and a plain WS listener, plus
// the app/pbx sequence wiring of testConfig.
func wsConfig(plainAddr, wsAddr, appURI, pbxURI string) config.Config {
	cfg := testConfig(plainAddr, appURI, pbxURI)
	cfg.WS = config.WS{Listen: wsAddr}
	return cfg
}

// wssConfig builds a config with a plain UDP listener and a secure WSS listener
// bound to rp, mirroring tlsConfig for the wss block.
func wssConfig(plainAddr, wssAddr, appURI, pbxURI string, rp config.ResolvedTLSProfile) config.Config {
	cfg := testConfig(plainAddr, appURI, pbxURI)
	cfg.WSS = config.WSS{Listen: wssAddr, TLSProfile: rp.Name, Resolved: &rp}
	return cfg
}

// startEngineWS starts the engine and blocks until every configured WS/WSS port has
// bound (the plain ready signal only covers the UDP socket). A WSS listener gets a
// real TLS provider; a plain-WS-only config needs none.
func startEngineWS(t *testing.T, cfg config.Config) *Engine {
	t.Helper()

	var opts []Option
	if cfg.WSS.Listen != "" {
		opts = append(opts, WithTLSProvider(tlsprov.NewStdProvider(nil)))
	}
	eng, err := New(cfg, opts...)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}

	waitEngineReady(t, eng, cfg)
	return eng
}

// ── behavior tests ───────────────────────────────────────────────────────────

// AC1/AC3: a real WebSocket SIP client places a call through the WS listener and it
// is bridged by the unchanged B2BUA handlers, exactly like a UDP caller.
func TestWebSocketListenerRoutesInvite(t *testing.T) {
	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)

	plainAddr := freeAddr(t)
	wsAddr := freeAddr(t)
	eng := startEngineWS(t, wsConfig(plainAddr, wsAddr, app.sipURI(), pbx.sipURI()))

	// The caller dials over ws (sipgo's ws client transport); the engine answers
	// the same way it does for UDP.
	caller := newFakeUAC(t)
	inviteAndWait200(t, caller, "sip:"+wsAddr+";transport=ws", app, pbx)

	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call after ws INVITE, got %d", n)
	}
}

// AC2: a secure WebSocket SIP client connects over TLS to the WSS listener and the
// signaling proceeds to a 200 OK end-to-end.
func TestWebSocketSecureListenerRoutesInvite(t *testing.T) {
	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)

	plainAddr := freeAddr(t)
	wssAddr := freeAddr(t)
	startEngineWS(t, wssConfig(plainAddr, wssAddr, app.sipURI(), pbx.sipURI(), serverProfile(t, false)))

	caller := newFakeUACTLS(t, &tls.Config{InsecureSkipVerify: true})
	inviteAndWait200(t, caller, "sip:"+wssAddr+";transport=wss", app, pbx)
}

// AC4 (rescoped): the WebSocket upgrade response selects the sip subprotocol — sipgo
// advertises and negotiates it natively on the transport.
func TestWebSocketNegotiatesSipSubprotocol(t *testing.T) {
	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)

	wsAddr := freeAddr(t)
	startEngineWS(t, wsConfig(freeAddr(t), wsAddr, app.sipURI(), pbx.sipURI()))

	dialer := ws.Dialer{Protocols: []string{"sip"}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, hs, err := dialer.Dial(ctx, "ws://"+wsAddr)
	if err != nil {
		t.Fatalf("ws upgrade: %v", err)
	}
	defer conn.Close()

	if hs.Protocol != "sip" {
		t.Fatalf("negotiated subprotocol = %q, want %q", hs.Protocol, "sip")
	}
}

// AC5: the UDP and WS listeners serve concurrently in the same engine run — a UDP
// caller and a WS caller each complete a call without disturbing the other.
func TestWebSocketAndUDPListenersServeConcurrently(t *testing.T) {
	app := newFakeUAS(t)
	pbx := newFakeUAS(t)

	plainAddr := freeAddr(t)
	wsAddr := freeAddr(t)
	eng := startEngineWS(t, wsConfig(plainAddr, wsAddr, app.sipURI(), pbx.sipURI()))

	// Plain UDP caller completes a full call (registered after ACK).
	completeCall(t, newFakeUAC(t), "sip:"+plainAddr, app, pbx)
	if n := eng.calls.len(); n != 1 {
		t.Fatalf("expected 1 active call after UDP INVITE, got %d", n)
	}

	// A ws caller drives the same handlers on the same engine; the UDP listener is
	// unperturbed.
	inviteAndWait200(t, newFakeUAC(t), "sip:"+wsAddr+";transport=ws", app, pbx)
}

// AC6: a config with no ws/wss block opens no WebSocket port — the engine holds no
// WSS server context and no WS listen address.
func TestNoWebSocketConfigBindsNoWebSocketPort(t *testing.T) {
	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)
	cfg := testConfig(freeAddr(t), app.sipURI(), pbx.sipURI())

	eng, err := New(cfg, WithTLSProvider(tlsprov.NewStdProvider(nil)))
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if eng.wssServerConf != nil {
		t.Fatal("expected nil wssServerConf with no wss.listen")
	}
	if eng.cfg.WS.Listen != "" {
		t.Fatalf("expected empty WS.Listen, got %q", eng.cfg.WS.Listen)
	}
}

// R1 fail-fast: a wss.listen with no provider, or with no resolved profile, aborts
// New rather than starting degraded.
func TestNewFailsFastOnWSSMisconfig(t *testing.T) {
	app := newFakeUASTCP(t)
	pbx := newFakeUAS(t)

	cfg := wssConfig(freeAddr(t), freeAddr(t), app.sipURI(), pbx.sipURI(), serverProfile(t, false))
	if _, err := New(cfg); err == nil {
		t.Fatal("expected error: wss.listen configured but no provider")
	}

	noResolved := cfg
	noResolved.WSS = config.WSS{Listen: cfg.WSS.Listen, TLSProfile: "listener", Resolved: nil}
	if _, err := New(noResolved, WithTLSProvider(tlsprov.NewStdProvider(nil))); err == nil {
		t.Fatal("expected error: wss.listen has no resolved profile")
	}
}
