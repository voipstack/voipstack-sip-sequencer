//go:build e2e

package e2e

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// listenerProfile mints a CA + a 127.0.0.1 server cert, writes the cert/key to a
// temp dir, and returns a yamlTLSProfile for a TLS-terminating listener (e.g. wss).
func listenerProfile(t *testing.T) yamlTLSProfile {
	t.Helper()
	dir := t.TempDir()
	caCK := mintCert(t, "wss-ca", nil, true, nil, nil)
	srvCK := mintCert(t, "sequencer", caCK, false, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, []net.IP{net.ParseIP("127.0.0.1")})

	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	writePEM(t, certPath, srvCK.certPEM())
	writePEM(t, keyPath, srvCK.keyPEM())
	return yamlTLSProfile{Cert: certPath, Key: keyPath, MinVersion: "tlsv1.2"}
}

// newSecureCaller is a client-only caller whose UA carries a TLS config, so it can
// dial a TLS-terminating listener (wss or inbound tls). It trusts any server cert
// (the listener cert is freshly minted per test); the point is the secure transport,
// not cert validation.
func newSecureCaller(t *testing.T) *fakeUAC {
	t.Helper()
	ua, err := sipgo.NewUA(sipgo.WithUserAgenTLSConfig(&tls.Config{InsecureSkipVerify: true})) //nolint:gosec // freshly minted self-signed listener cert under test
	if err != nil {
		t.Fatalf("secure caller UA: %v", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		t.Fatalf("secure caller client: %v", err)
	}
	contact := sip.ContactHeader{Address: sip.Uri{Host: "127.0.0.1", Port: 5060}}
	return &fakeUAC{dcc: sipgo.NewDialogClientCache(cli, contact)}
}

// newSecureWSCaller is the secure caller, named for WSS scenarios.
func newSecureWSCaller(t *testing.T) *fakeUAC { return newSecureCaller(t) }

// Given a plain WebSocket listener; When a SIP-over-WS client places a call; Then it
// is bridged exactly like a UDP caller and connects end to end.
func TestWebSocketCallerConnects(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newFakeUAC(t)

	serveApp(t, appSrv, "A", nil, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "none", "abort")})
	cfg.WSListen = freeTCPPort(t)
	startReady(t, cfg)

	waitTCPListening(t, cfg.WSListen, 5*time.Second)
	connectTo(t, caller, "sip:"+cfg.WSListen+";transport=ws")
}

// Given a secure WebSocket listener bound to a TLS profile; When a SIP-over-WSS
// client connects over TLS; Then signaling proceeds to a 200 OK end to end.
func TestSecureWebSocketCallerConnects(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)
	caller := newSecureWSCaller(t)

	serveApp(t, appSrv, "A", nil, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "none", "abort")})
	cfg.WSSListen = freeTCPPort(t)
	cfg.WSSProfile = "wsslistener"
	cfg.TLSProfiles = map[string]yamlTLSProfile{"wsslistener": listenerProfile(t)}
	startReady(t, cfg)

	waitTCPListening(t, cfg.WSSListen, 5*time.Second)
	connectTo(t, caller, "sip:"+cfg.WSSListen+";transport=wss")
}

// Given a UDP and a WS listener in the same engine; When a UDP caller and a WS
// caller each place a call; Then both connect without disturbing the other.
func TestWebSocketAndUDPListenersConcurrent(t *testing.T) {
	appSrv := newFakeUAS(t)
	pbx := newFakeUAS(t)

	serveApp(t, appSrv, "A", nil, 200, "OK", []byte(testSDP2))
	autoAnswer(t, pbx, []byte(testSDP2))

	cfg := baseConfig(t, pbx, []yamlApp{app("A", appSrv, "none", "abort")})
	cfg.WSListen = freeTCPPort(t)
	s := startReady(t, cfg)
	waitTCPListening(t, cfg.WSListen, 5*time.Second)

	// UDP caller completes a full INVITE/ACK (its Contact is the UDP address).
	establish(t, newFakeUAC(t), s.sipListen)
	// WS caller on the same engine reaches 200 OK (no ACK — see connectTo).
	connectTo(t, newFakeUAC(t), "sip:"+cfg.WSListen+";transport=ws")
}
