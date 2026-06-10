package tlsprov

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voipstack/voipstack-sip-sequencer/internal/config"
)

// writeKeyPair generates a self-signed RSA cert + key into dir and returns their
// paths. When encrypted, the key is written as a legacy PKCS#1 (DEK-Info) PEM
// encrypted with pass — the only encrypted form the stdlib can decrypt.
func writeKeyPair(t *testing.T, dir, cn string, encrypted bool, pass string) (certPath, keyPath string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     time.Unix(1<<31, 0),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	certPath = filepath.Join(dir, cn+"-cert.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}

	keyDER := x509.MarshalPKCS1PrivateKey(key)
	var keyBlock *pem.Block
	if encrypted {
		keyBlock, err = x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", keyDER, []byte(pass), x509.PEMCipherAES256) //nolint:staticcheck // SA1019: legacy encrypted PEM fixture
		if err != nil {
			t.Fatalf("encrypt key: %v", err)
		}
	} else {
		keyBlock = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER}
	}
	keyPath = filepath.Join(dir, cn+"-key.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(keyBlock), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

// captureHandler is a real slog.Handler fake that records every emitted record so a
// test can assert on log content (no internal code mocked).
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) dump() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var b strings.Builder
	for _, r := range h.records {
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			b.WriteString(" " + a.Key + "=" + a.Value.String())
			return true
		})
		b.WriteString("\n")
	}
	return b.String()
}

func newCapturing() (*StdProvider, *captureHandler) {
	h := &captureHandler{}
	return NewStdProvider(slog.New(h)), h
}

// AC1: a valid unencrypted cert/key loads into usable Material.
func TestLoadUnencryptedCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeKeyPair(t, dir, "plain", false, "")
	p := NewStdProvider(nil)

	m, err := p.Load(config.ResolvedTLSProfile{Name: "in", Cert: certPath, Key: keyPath})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Certificate.PrivateKey == nil || len(m.Certificate.Certificate) == 0 {
		t.Fatalf("certificate not usable: %+v", m.Certificate)
	}
}

// AC2: a legacy PKCS#1 encrypted key + correct passphrase loads.
func TestDecryptLegacyEncryptedKeyWithPassphrase(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeKeyPair(t, dir, "enc", true, "s3cret")
	p := NewStdProvider(nil)

	m, err := p.Load(config.ResolvedTLSProfile{Name: "in", Cert: certPath, Key: keyPath, Passphrase: "s3cret"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Certificate.PrivateKey == nil {
		t.Fatal("decrypted certificate has no private key")
	}
}

// AC3: a wrong passphrase fails clearly with no material and no secret in the error.
func TestWrongPassphraseFailsClearly(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeKeyPair(t, dir, "enc", true, "s3cret")
	p := NewStdProvider(nil)

	m, err := p.Load(config.ResolvedTLSProfile{Name: "in", Cert: certPath, Key: keyPath, Passphrase: "wrong"})
	if err == nil {
		t.Fatal("expected error for wrong passphrase")
	}
	if m != nil {
		t.Fatal("expected no material on failure")
	}
	if !strings.Contains(err.Error(), "could not decrypt") {
		t.Fatalf("error missing reason: %v", err)
	}
	if strings.Contains(err.Error(), "wrong") || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("error leaked passphrase: %v", err)
	}
}

// AC3 sibling: a PKCS#8 ENCRYPTED PRIVATE KEY is explicitly unsupported.
func TestPKCS8EncryptedKeyUnsupported(t *testing.T) {
	dir := t.TempDir()
	certPath, _ := writeKeyPair(t, dir, "p8", false, "")
	keyPath := filepath.Join(dir, "p8-key.pem")
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("not-real-der")})
	if err := os.WriteFile(keyPath, pkcs8, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	p := NewStdProvider(nil)

	m, err := p.Load(config.ResolvedTLSProfile{Name: "in", Cert: certPath, Key: keyPath, Passphrase: "x"})
	if err == nil {
		t.Fatal("expected unsupported error")
	}
	if m != nil {
		t.Fatal("expected no material")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error not actionable: %v", err)
	}
}

// AC4: a missing cert file fails with the path named and emits a secret-free audit line.
func TestMissingCertFileFailsNamed(t *testing.T) {
	p, h := newCapturing()
	missing := filepath.Join(t.TempDir(), "nope.pem")

	_, err := p.Load(config.ResolvedTLSProfile{Name: "in", Cert: missing, Key: missing})
	if err == nil {
		t.Fatal("expected error for missing cert")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("error does not name path: %v", err)
	}
	logs := h.dump()
	if !strings.Contains(logs, "tls certificate load failed") ||
		!strings.Contains(logs, "profile=in") || !strings.Contains(logs, missing) {
		t.Fatalf("audit line missing profile/path: %q", logs)
	}
}

// AC5: two endpoints' profiles naming the same cert read disk once (same *Material).
func TestSharedProfileReadOnce(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeKeyPair(t, dir, "shared", false, "")
	p := NewStdProvider(nil)

	m1, err := p.Load(config.ResolvedTLSProfile{Name: "a", Cert: certPath, Key: keyPath})
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	m2, err := p.Load(config.ResolvedTLSProfile{Name: "b", Cert: certPath, Key: keyPath})
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if m1 != m2 {
		t.Fatal("expected the cached *Material to be reused")
	}
}

// AC6: a CA bundle of two CAs yields a non-empty trust pool with both subjects.
func TestCABundleLoadedIntoTrustPool(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeKeyPair(t, dir, "leaf", false, "")
	ca1, _ := writeKeyPair(t, dir, "ca-one", false, "")
	ca2, _ := writeKeyPair(t, dir, "ca-two", false, "")

	b1, _ := os.ReadFile(ca1)
	b2, _ := os.ReadFile(ca2)
	caPath := filepath.Join(dir, "bundle.pem")
	if err := os.WriteFile(caPath, append(b1, b2...), 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}

	p := NewStdProvider(nil)
	m, err := p.Load(config.ResolvedTLSProfile{Name: "in", Cert: certPath, Key: keyPath, CA: caPath})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.TrustPool == nil {
		t.Fatal("expected non-nil trust pool")
	}
	if got := len(m.TrustPool.Subjects()); got != 2 { //nolint:staticcheck // SA1019: Subjects on file-built pool is fine in test
		t.Fatalf("expected 2 CA subjects, got %d", got)
	}
}

// AC6 negative: an unparseable CA bundle is a named load error, not a silent empty pool.
func TestEmptyCABundleFailsNamed(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeKeyPair(t, dir, "leaf", false, "")
	caPath := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(caPath, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("write junk: %v", err)
	}

	p := NewStdProvider(nil)
	m, err := p.Load(config.ResolvedTLSProfile{Name: "in", Cert: certPath, Key: keyPath, CA: caPath})
	if err == nil {
		t.Fatal("expected error for unparseable CA bundle")
	}
	if m != nil {
		t.Fatal("expected no material")
	}
	if !strings.Contains(err.Error(), "no valid CA certificates") || !strings.Contains(err.Error(), caPath) {
		t.Fatalf("error not named: %v", err)
	}
}

// AC7: no certificate body, private key, or passphrase appears in any log line.
func TestNoSecretMaterialInLogs(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeKeyPair(t, dir, "secret", true, "topsecretpass")
	keyBytes, _ := os.ReadFile(keyPath)
	certBytes, _ := os.ReadFile(certPath)

	p, h := newCapturing()
	// success path
	if _, err := p.Load(config.ResolvedTLSProfile{Name: "ok", Cert: certPath, Key: keyPath, Passphrase: "topsecretpass"}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	// failure path (wrong passphrase) on a fresh path pair to avoid the cache
	cert2, key2 := writeKeyPair(t, dir, "secret2", true, "anotherpass")
	if _, err := p.Load(config.ResolvedTLSProfile{Name: "bad", Cert: cert2, Key: key2, Passphrase: "incorrect"}); err == nil {
		t.Fatal("expected failure")
	}

	logs := h.dump()
	for _, secret := range []string{"topsecretpass", "anotherpass", "incorrect", string(bytes.TrimSpace(keyBytes)), string(bytes.TrimSpace(certBytes))} {
		if secret != "" && strings.Contains(logs, secret) {
			t.Fatalf("log leaked secret material: %q", logs)
		}
	}
}

// TestLoadAllNoTLSIsNoop: a plain config references no profiles → nil, nothing loaded.
func TestLoadAllNoTLSIsNoop(t *testing.T) {
	p := NewStdProvider(nil)
	if err := LoadAll(config.Config{}, p); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
}

// TestLoadAllAbortsOnBadProfile: one unloadable referenced profile → error returned.
func TestLoadAllAbortsOnBadProfile(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeKeyPair(t, dir, "good", false, "")
	missing := filepath.Join(dir, "missing.pem")

	cfg := config.Config{
		TLS:     config.TLS{Resolved: &config.ResolvedTLSProfile{Name: "in", Cert: certPath, Key: keyPath}},
		NextHop: config.NextHop{Resolved: &config.ResolvedTLSProfile{Name: "next", Cert: missing, Key: missing}},
	}
	p := NewStdProvider(nil)
	if err := LoadAll(cfg, p); err == nil {
		t.Fatal("expected LoadAll to abort on bad profile")
	}
}
