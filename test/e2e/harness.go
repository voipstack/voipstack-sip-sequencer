//go:build e2e

// Package e2e drives the compiled sip-sequencer binary as a black box: it builds
// the real artifact, starts it as a subprocess with a generated YAML config, and
// exercises it over real SIP/HTTP sockets using sipgo fakes. It asserts the whole
// service works end to end — the binary, the config parser, the listeners, media
// anchoring, and graceful shutdown — none of which the in-process tests in
// internal/b2bua cover.
package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// ── building the binary ──────────────────────────────────────────────────────

// buildBinary compiles cmd/sip-sequencer once to a temp dir and returns the
// binary path plus a cleanup func. It locates the module root by walking up from
// the working dir to the dir containing go.mod, so the build resolves regardless
// of the test's working directory.
func buildBinary() (path string, cleanup func(), err error) {
	root, err := moduleRoot()
	if err != nil {
		return "", nil, err
	}

	dir, err := os.MkdirTemp("", "sip-sequencer-e2e-")
	if err != nil {
		return "", nil, fmt.Errorf("mkdir temp: %w", err)
	}
	out := filepath.Join(dir, "sip-sequencer")

	cmd := exec.Command("go", "build", "-o", out, "./cmd/sip-sequencer")
	cmd.Dir = root
	if combined, buildErr := cmd.CombinedOutput(); buildErr != nil {
		os.RemoveAll(dir)
		return "", nil, fmt.Errorf("go build: %w\n%s", buildErr, combined)
	}
	return out, func() { os.RemoveAll(dir) }, nil
}

// moduleRoot walks up from the current working directory until it finds go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// ── free ports ───────────────────────────────────────────────────────────────

// freeUDPPort binds udp 127.0.0.1:0, reads the port, and releases it. There is a
// small TOCTOU race before the subprocess re-binds; acceptable in test contexts
// and identical to the race accepted by the in-process tests' freeAddr.
func freeUDPPort(tb testing.TB) string {
	tb.Helper()
	l, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("freeUDPPort: %v", err)
	}
	addr := l.LocalAddr().String()
	l.Close()
	return addr
}

// freeTCPPort binds tcp 127.0.0.1:0, reads the port, and releases it.
func freeTCPPort(tb testing.TB) string {
	tb.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("freeTCPPort: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// freeRTPRange returns "min-max" with an even min and a small span. The engine's
// port parser requires even bounds; a tiny span keeps the test from reserving a
// broad block.
func freeRTPRange(tb testing.TB) string {
	tb.Helper()
	return freeRTPRangeSpan(tb, 10)
}

// freeRTPRangeSpan returns an even-bounded "min-max" with the given span, chosen
// from a low fixed window BELOW the OS ephemeral pool (32768–60999 on Linux). The
// engine binds a UDP socket for every pair in this range; if the range overlapped
// the ephemeral pool, the engine's or the test's own ephemeral sockets would land
// inside it and bind() would collide (engine returns 500 "media bind failed") — most
// visibly under concurrent calls. A low dedicated window is collision-free.
//
// Tests in this package run sequentially (no t.Parallel) and each subprocess is
// SIGTERM'd on cleanup, so reusing the window across tests is safe. Two simultaneous
// `go test` runs of this one package could collide on it; acceptable for a helper.
func freeRTPRangeSpan(tb testing.TB, span int) string {
	tb.Helper()
	if span%2 != 0 {
		span++
	}
	for base := 20000; base+span < 30000; base += span + 2 {
		l, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: base})
		if err != nil {
			continue // base in use; try the next window
		}
		l.Close()
		return fmt.Sprintf("%d-%d", base, base+span)
	}
	tb.Fatal("freeRTPRangeSpan: no free low RTP window")
	return ""
}

// deadAddr reserves a TCP port and releases it, yielding a SIP URI that refuses
// connections — used to model an unreachable application server.
func deadAddr(tb testing.TB) string {
	tb.Helper()
	return "sip:" + freeTCPPort(tb)
}

// waitTCPListening blocks until a TCP connection to addr succeeds, used to confirm
// a WS/WSS listener has bound (the /health probe only proves the HTTP socket is up,
// not the SIP listeners that bind in the same group).
func waitTCPListening(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("listener %s not accepting within %s", addr, timeout)
}

// ── YAML config ──────────────────────────────────────────────────────────────

type yamlApp struct {
	Name       string `yaml:"name"`
	URI        string `yaml:"uri"`
	Transport  string `yaml:"transport,omitempty"`
	TLSProfile string `yaml:"tls_profile,omitempty"`
	Media      string `yaml:"media,omitempty"`
	OnFailure  string `yaml:"on_failure,omitempty"`
}

// yamlTLSProfile is a named TLS policy referenced by an app or the next hop.
type yamlTLSProfile struct {
	Cert       string
	Key        string
	CA         string
	MinVersion string
}

// yamlConfig is the typed source for the generated config file. Marshalling from
// a typed struct (rather than string templating) keeps the output gofmt-clean and
// structurally incapable of emitting a key the strict config parser would reject.
type yamlConfig struct {
	SIPListen        string
	NextHopURI       string
	NextHopTransport string
	RTPRange         string
	ObsListen        string
	LogLevel         string
	Apps             []yamlApp
	TLSProfiles      map[string]yamlTLSProfile
	WSListen         string // plain WebSocket listener (ws.listen)
	WSSListen        string // secure WebSocket listener (wss.listen)
	WSSProfile       string // tls_profile bound to the wss listener
	TLSListen        string // inbound SIP-over-TLS listener (tls.listen)
	TLSProfile       string // tls_profile bound to the inbound TLS listener
}

// marshalYAML renders cfg into the exact shape the binary accepts. Nested mappings
// are built explicitly so sequence is always present (a strict required key) and
// next_hop is a mapping with uri (the bare-string form is rejected).
func (c yamlConfig) marshalYAML() ([]byte, error) {
	doc := map[string]any{
		"sip": map[string]any{"listen": c.SIPListen},
		"next_hop": map[string]any{
			"uri":       c.NextHopURI,
			"transport": c.NextHopTransport,
		},
		"rtp":      map[string]any{"port_range": c.RTPRange},
		"sequence": appsToYAML(c.Apps),
	}
	if c.ObsListen != "" {
		doc["observability"] = map[string]any{"listen": c.ObsListen}
	}
	if c.TLSListen != "" {
		doc["tls"] = map[string]any{"listen": c.TLSListen, "tls_profile": c.TLSProfile}
	}
	if c.WSListen != "" {
		doc["ws"] = map[string]any{"listen": c.WSListen}
	}
	if c.WSSListen != "" {
		doc["wss"] = map[string]any{"listen": c.WSSListen, "tls_profile": c.WSSProfile}
	}
	if c.LogLevel != "" {
		doc["log_level"] = c.LogLevel
	}
	if len(c.TLSProfiles) > 0 {
		profiles := map[string]any{}
		for name, p := range c.TLSProfiles {
			m := map[string]any{"cert": p.Cert, "key": p.Key}
			if p.CA != "" {
				m["ca"] = p.CA
			}
			if p.MinVersion != "" {
				m["min_version"] = p.MinVersion
			}
			profiles[name] = m
		}
		doc["tls_profiles"] = profiles
	}
	return yaml.Marshal(doc)
}

func appsToYAML(apps []yamlApp) []map[string]any {
	out := make([]map[string]any, 0, len(apps))
	for _, a := range apps {
		m := map[string]any{"name": a.Name, "uri": a.URI}
		if a.Transport != "" {
			m["transport"] = a.Transport
		}
		if a.TLSProfile != "" {
			m["tls_profile"] = a.TLSProfile
		}
		if a.Media != "" {
			m["media"] = a.Media
		}
		if a.OnFailure != "" {
			m["on_failure"] = a.OnFailure
		}
		out = append(out, m)
	}
	return out
}

// writeConfig marshals cfg and writes it to dir/config.yaml, returning the path.
func writeConfig(t *testing.T, dir string, cfg yamlConfig) string {
	t.Helper()
	body, err := cfg.marshalYAML()
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// writeRawConfig writes verbatim YAML bytes, for negative tests that feed the
// binary a config the typed generator could not produce (e.g. an unknown key).
func writeRawConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write raw config: %v", err)
	}
	return path
}

// ── subprocess lifecycle ─────────────────────────────────────────────────────

type sequencer struct {
	cmd       *exec.Cmd
	sipListen string
	obsListen string
	stderr    *bytes.Buffer
	done      chan struct{} // closed once the process is reaped
	exitErr   error         // set before done is closed; read after
}

// start launches the prebuilt binary with the given config path. It streams the
// child's stderr into a buffer (and the test log) and reaps the process in the
// background. It does not wait for readiness — callers use waitReady or inspect
// exit directly (negative tests).
func start(t *testing.T, cfgPath, sipListen, obsListen string) *sequencer {
	t.Helper()
	if binPath == "" {
		t.Fatal("binPath empty: TestMain did not build the binary")
	}

	stderr := &bytes.Buffer{}
	cmd := exec.Command(binPath, "-config", cfgPath)
	cmd.Stderr = io.MultiWriter(stderr, testLogWriter{t})

	if err := cmd.Start(); err != nil {
		t.Fatalf("start subprocess: %v", err)
	}

	s := &sequencer{
		cmd:       cmd,
		sipListen: sipListen,
		obsListen: obsListen,
		stderr:    stderr,
		done:      make(chan struct{}),
	}
	// Reap once: store the exit error, then close done so every waiter
	// (waitReady, waitExit, stop) observes it without competing for a value.
	go func() {
		s.exitErr = cmd.Wait()
		close(s.done)
	}()

	t.Cleanup(s.stop)
	return s
}

// startReady starts the binary and blocks until /health reports 200. The
// subprocess-owned ports (sip.listen, observability.listen) are picked pick-close-
// rebind, so a rare TOCTOU race can let another socket grab one before the child
// binds it, exiting the child with "address already in use". On a non-ready start it
// re-picks those ports and relaunches a few times before failing.
func startReady(t *testing.T, cfg yamlConfig) *sequencer {
	t.Helper()
	const attempts = 3
	var lastStderr string
	for i := 0; i < attempts; i++ {
		dir := t.TempDir()
		cfgPath := writeConfig(t, dir, cfg)
		s := start(t, cfgPath, cfg.SIPListen, cfg.ObsListen)
		if s.tryReady(5 * time.Second) {
			return s
		}
		lastStderr = s.stderr.String()
		s.stop() // ensure the failed child is gone before re-picking ports
		cfg.SIPListen = freeUDPPort(t)
		cfg.ObsListen = freeTCPPort(t)
	}
	t.Fatalf("sequencer not ready after %d attempts\nlast stderr:\n%s", attempts, lastStderr)
	return nil
}

// tryReady polls /health until it returns 200, returning false (rather than failing
// the test) if the process exits early or the timeout elapses — so startReady can
// retry. The whole window is bounded by timeout.
func (s *sequencer) tryReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	url := "http://" + s.obsListen + "/health"
	for time.Now().Before(deadline) {
		select {
		case <-s.done: // exited before becoming ready
			return false
		default:
		}
		if status, body, err := httpGet(url); err == nil && status == http.StatusOK && body == "ok" {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// stop sends SIGTERM (the signal main.go traps for graceful Shutdown), waits
// bounded for exit, then SIGKILLs on timeout.
func (s *sequencer) stop() {
	if s.cmd.Process == nil {
		return
	}
	select {
	case <-s.done: // already exited (e.g. a negative test reaped it)
		return
	default:
	}
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
	}
}

// waitExit returns the process exit error (nil on clean exit) or fails if the
// process is still running after timeout. Used by negative tests.
func (s *sequencer) waitExit(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case <-s.done:
		return s.exitErr
	case <-time.After(timeout):
		t.Fatalf("process did not exit within %s\nstderr:\n%s", timeout, s.stderr.String())
		return nil
	}
}

// ── HTTP helpers ─────────────────────────────────────────────────────────────

func httpGet(url string) (status int, body string, err error) {
	resp, err := http.Get(url) //nolint:gosec // fixed localhost URL under test
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(b), nil
}

//nolint:unused // used by the metrics scenario
func mustGet(t *testing.T, url string) (int, string) {
	t.Helper()
	status, body, err := httpGet(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return status, body
}

// ── test log plumbing ────────────────────────────────────────────────────────

// testLogWriter forwards the subprocess's stderr lines to t.Logf so a failing
// run shows the binary's own logs inline.
type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Logf("[sequencer] %s", bytes.TrimRight(p, "\n"))
	return len(p), nil
}
