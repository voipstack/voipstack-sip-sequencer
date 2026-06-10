# TLS Provider Boundary & Certificate Loading (STORY-001-013)

## Requirements

Introduce a single swappable TLS boundary — the `Provider` — that isolates the TLS library
(`crypto/tls` + `crypto/x509`) from the listener, dialers, and config parsing, and implement
its first responsibility: load a resolved `tls_profile`'s certificate (cert + key PEM, optional
passphrase for encrypted keys, optional CA bundle) into usable key material and a trust pool.
Loading happens eagerly at startup so an unloadable certificate aborts the process before any
listener binds; a certificate referenced by several endpoints is read from disk once and reused.
No certificate, private-key, or passphrase material ever appears in logs.

Boundary: load and cache certificate material only. Building TLS contexts / verification policy
(STORY-001-014), opening listeners (015), and dialing (016) are out of scope and consume this
boundary.

> **DEPENDENCY DECISION (settled): standard library only — zero new dependencies.** Encrypted
> keys are supported in the **legacy PKCS#1 PEM** form (`-----BEGIN RSA/EC PRIVATE KEY-----`
> with a `DEK-Info:` header) via stdlib `x509.DecryptPEMBlock`. Modern **PKCS#8-encrypted** keys
> (`-----BEGIN ENCRYPTED PRIVATE KEY-----`) are **not supported** — the stdlib cannot decrypt the
> DER-embedded `EncryptedPrivateKeyInfo`, and the Go team declined to add it. Such a key fails
> the load with a clear, actionable error (convert to an unencrypted or legacy-encrypted key).
> Unencrypted keys (any algorithm) are fully supported with no dependency.

## Entities

```mermaid
classDiagram
direction TB

class Provider {
    <<interface>>
    +Load(rp ResolvedTLSProfile) (*Material, error)
}

class StdProvider {
    -mu sync.Mutex
    -cache map~string~*Material
    -log *slog.Logger
    +Load(rp ResolvedTLSProfile) (*Material, error)
}

class Material {
    +tls.Certificate Certificate
    +*x509.CertPool TrustPool
}

class ResolvedTLSProfile {
    +string Name
    +string Cert
    +string Key
    +string Passphrase
    +string CA
    +...policy fields
}

Provider <|.. StdProvider : implements
StdProvider --> Material : builds + caches
ResolvedTLSProfile --> Provider : input
StdProvider ..> ResolvedTLSProfile : reads cert/key/passphrase/ca
```

Notes:
- `ResolvedTLSProfile` already exists in `internal/config` (STORY-001-012) — consumed as-is,
  not modified. The provider depends on it; it never re-parses YAML.
- `Material` is the loaded artifact for one profile: parsed key material + an optional trust
  pool. It is the std-provider's concrete type; STORY-001-014 (same package) consumes it to
  build `*tls.Config`. The swappable seam is the `Provider` interface, not `Material`.
- No DTOs, no new config types. Conservative: reuse `config.ResolvedTLSProfile` verbatim.

## Approach

1. **New package `internal/tlsprov`:**
   - Owns the `Provider` interface, the default `StdProvider` (over `crypto/tls`+`crypto/x509`),
     the `Material` type, the certificate cache, and the eager `LoadAll` startup helper.
   - The only package allowed to import `crypto/tls`/`crypto/x509`; keeps the swappable boundary
     intact (an OpenSSL/HSM provider implements the same interface). No new external dependency.

2. **Loading (default impl, stdlib only):**
   - Unencrypted key: `tls.X509KeyPair(certPEM, keyPEM)`.
   - Encrypted key (passphrase set): decode the key PEM block; if type is
     `ENCRYPTED PRIVATE KEY` (PKCS#8) → return a clear "PKCS#8-encrypted keys are not supported"
     error (stdlib cannot decrypt it). If `x509.IsEncryptedPEMBlock(block)` (legacy PKCS#1
     `DEK-Info`) → `x509.DecryptPEMBlock(block, pass)`, re-wrap the decrypted DER as a PEM block
     of the same type, then `tls.X509KeyPair`. Wrong passphrase → clear "could not decrypt private
     key" error, no material.
   - CA bundle: read `rp.CA`, `x509.NewCertPool()`, `AppendCertsFromPEM`; `false` → load error.

3. **Caching (load once, reuse):**
   - `cache map[string]*Material` guarded by a `sync.Mutex`, keyed by the cleaned cert+key path
     pair (`filepath.Clean(cert)+"\x00"+filepath.Clean(key)`). A profile (or two profiles) naming
     the same files reads disk once. Single owner, narrow interface (AGENTS.md).

4. **Eager startup orchestration (`LoadAll`):**
   - `LoadAll(cfg, provider, log)` collects every non-nil resolved profile referenced
     (`cfg.TLS.Resolved`, each `cfg.Sequence[i].Resolved`, `cfg.NextHop.Resolved`), calls
     `provider.Load` on each, and returns the first error (already audit-logged). A config with
     no TLS profiles is a no-op — plain configs carry zero TLS overhead.
   - Wire in `cmd/sip-sequencer/main.go` after `config.Load` and before `b2bua.New`/`Run`: on
     error, print to stderr and exit non-zero (mirrors `main.go:29`/`main.go:49`). Pass the
     provider into `b2bua.New` via a new `WithTLSProvider` option so STORY-001-014/015/016 can
     reach it; the engine stores it but does not yet use it in this story.

5. **No GlobalExceptionHandler / no service-controller layering** — Go errors are values, wrapped
   with `fmt.Errorf("...: %w")`, naming the profile and file path, never key/passphrase bytes.

## Structure

### Type/Interface Relationships
1. `Provider` is a small consumer-facing interface (one method this story) — same idiom as the
   existing `MetricsSink` seam in `internal/b2bua`.
2. `StdProvider` implements `Provider`; constructed by `NewStdProvider(log *slog.Logger) *StdProvider`.
3. `Material` is a plain value type carrying `tls.Certificate` + `*x509.CertPool`.
4. No inheritance; composition only. No new abstraction beyond the single boundary (YAGNI).

### Dependencies
1. `internal/tlsprov` imports `crypto/tls`, `crypto/x509`, `encoding/pem`, `os`, `path/filepath`,
   `sync`, `log/slog`, and `internal/config` (for `ResolvedTLSProfile`). No external dependency.
2. `cmd/sip-sequencer/main.go` depends on `internal/tlsprov` (construct provider + `LoadAll`).
3. `internal/b2bua` gains a `WithTLSProvider` option storing a `tlsprov.Provider`; no other change.
4. `internal/config` is unchanged and MUST stay free of `crypto/tls`.

### Layering
1. Boundary layer: `Provider` interface (swappable).
2. Default impl layer: `StdProvider` — file read, PEM decode, decrypt, key-pair assembly, CA pool.
3. Cache layer: mutex-guarded path-keyed `map[string]*Material`.
4. Startup-orchestration layer: `LoadAll` invoked from `main.go` (fail-fast before bind).

## Operations

### Create Package — `internal/tlsprov/provider.go`
1. Responsibility: define the boundary and the loaded artifact.
2. Define `type Material struct { Certificate tls.Certificate; TrustPool *x509.CertPool }`.
3. Define `type Provider interface { Load(rp config.ResolvedTLSProfile) (*Material, error) }`.
4. Define `type StdProvider struct { mu sync.Mutex; cache map[string]*Material; log *slog.Logger }`
   and `func NewStdProvider(log *slog.Logger) *StdProvider` initializing the cache (use a non-nil
   `log`, defaulting to `slog.Default()` when nil).

### Implement `StdProvider.Load` — `internal/tlsprov/provider.go`
1. Signature: `func (p *StdProvider) Load(rp config.ResolvedTLSProfile) (*Material, error)`.
2. Logic:
   - `key := filepath.Clean(rp.Cert) + "\x00" + filepath.Clean(rp.Key)`.
   - Lock `p.mu` (defer unlock). If `m, ok := p.cache[key]; ok` → return `m, nil`.
   - Read cert PEM (`os.ReadFile(rp.Cert)`) and key PEM (`os.ReadFile(rp.Key)`); on error →
     `p.auditFail(rp, pathThatFailed, err)` then return wrapped error.
   - Build `tls.Certificate`:
     - If `rp.Passphrase == ""` → `cert, err := tls.X509KeyPair(certPEM, keyPEM)`.
     - Else → `decPEM, err := decryptKeyPEM(p.log, keyPEM, rp.Passphrase)`; on success
       `tls.X509KeyPair(certPEM, decPEM)`. On decrypt/unsupported failure → audit + return the
       (secret-free) error from `decryptKeyPEM`, wrapped with `tls_profiles[%q]` and `rp.Name`.
   - If `rp.CA != ""`: read CA PEM; `pool := x509.NewCertPool()`; if `!pool.AppendCertsFromPEM(caPEM)`
     → audit + `fmt.Errorf("tls_profiles[%q]: no valid CA certificates in %q", rp.Name, rp.CA)`.
     Else `m.TrustPool = pool`.
   - `m := &Material{Certificate: cert, TrustPool: pool}`; `p.cache[key] = m`; return `m, nil`.
3. Constraints: never include cert/key/passphrase bytes in any error or log; cache write happens
   exactly once per path pair under the mutex (race-clean).

### Implement key decryption helper — `internal/tlsprov/decrypt.go`
1. Signature: `func decryptKeyPEM(log *slog.Logger, keyPEM []byte, passphrase string) (decryptedPEM []byte, err error)`.
   The `log` is threaded in so the "passphrase set but key is not encrypted" `Debug` note (step 2,
   final branch) is emitted by the helper that detects the condition, not by the caller.
2. Logic (stdlib only):
   - `block, _ := pem.Decode(keyPEM)`; if `block == nil` → error "no PEM block in key".
   - If `block.Type == "ENCRYPTED PRIVATE KEY"` (PKCS#8 encrypted) → return
     `errors.New("PKCS#8-encrypted private keys are not supported; provide an unencrypted key or a legacy (PKCS#1, DEK-Info) encrypted key")`.
     (No decrypt attempt — the stdlib cannot.)
   - Else if `x509.IsEncryptedPEMBlock(block)` (legacy PKCS#1 `DEK-Info`):
     `der, err := x509.DecryptPEMBlock(block, []byte(passphrase))`; on error → generic
     `errors.New("could not decrypt private key")` (no bytes). On success → return
     `pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: der})` (re-wrap as the same key type,
     now unencrypted, ready for `tls.X509KeyPair`).
   - Else (passphrase set but key is not encrypted): log a `Debug` note "passphrase set but key is
     not encrypted" and return the original `keyPEM` unchanged (ignore, don't fail).
3. Note: `x509.DecryptPEMBlock`/`IsEncryptedPEMBlock` are deprecated (SA1019 under staticcheck) but
   present and functional; the repo's DoD runs `go vet`, which does not flag deprecation.

### Implement audit helper — `internal/tlsprov/provider.go`
1. Signature: `func (p *StdProvider) auditFail(rp config.ResolvedTLSProfile, path string, err error)`.
2. Logic: `p.log.Error("tls certificate load failed", "profile", rp.Name, "path", path)`. Do NOT
   log `err` if it could carry bytes — log only the profile, path, and a static message; the
   detailed (sanitized) error is returned to the caller, not logged with secrets.

### Implement `LoadAll` — `internal/tlsprov/load_all.go`
1. Signature: `func LoadAll(cfg config.Config, p Provider) error`.
2. Logic:
   - Collect resolved profiles into `[]*config.ResolvedTLSProfile`: `cfg.TLS.Resolved`,
     `cfg.Sequence[i].Resolved` for each i, `cfg.NextHop.Resolved`; skip nils.
   - For each, `if _, err := p.Load(*rp); err != nil { return err }`.
   - Empty list → return nil (no-op for plain configs).
3. Constraints: deduplication is handled by the provider cache; `LoadAll` may pass the same
   profile/path more than once safely.

### Wire startup — `cmd/sip-sequencer/main.go`
1. After `cfg, err := config.Load(...)` succeeds and the logger is configured, construct
   `provider := tlsprov.NewStdProvider(slog.Default())`.
2. `if err := tlsprov.LoadAll(cfg, provider); err != nil { fmt.Fprintf(os.Stderr, "error: %v\n", err); os.Exit(1) }`
   — before `b2bua.New`, so an unloadable cert aborts before any listener binds.
3. Pass the provider into the engine: `opts = append(opts, b2bua.WithTLSProvider(provider))`.

### Add engine option — `internal/b2bua/engine.go`
1. Add field `tlsProvider tlsprov.Provider` to `Engine`.
2. Add `func WithTLSProvider(p tlsprov.Provider) Option { return func(e *Engine) { e.tlsProvider = p } }`.
3. Store only; no use yet (consumed in STORY-001-014/015/016). Ensure no import cycle
   (`b2bua` → `tlsprov` → `config`; `tlsprov` must not import `b2bua`).

### Add tests — `internal/tlsprov/provider_test.go`
1. BDD Given/When/Then, real cert files in `t.TempDir()` generated by a test helper (no committed
   secrets, no mocks — AGENTS.md). Helper `writeKeyPair(t, dir, cn string, encrypted bool, pass string)`
   uses stdlib only to make a self-signed RSA cert + key (the `cn` common-name also prefixes the
   written filenames so several pairs share one `t.TempDir()` without collision); when `encrypted`,
   marshal the key with
   `x509.MarshalPKCS1PrivateKey` and `x509.EncryptPEMBlock(rand.Reader, "RSA PRIVATE KEY", der, pass, x509.PEMCipherAES256)`
   to produce a legacy PKCS#1 (`DEK-Info`) encrypted PEM — round-trips with `DecryptPEMBlock`.
2. Cover AC1–AC7:
   - `TestLoadUnencryptedCertificate` (AC1): valid cert/key → `Material.Certificate` usable.
   - `TestDecryptLegacyEncryptedKeyWithPassphrase` (AC2): legacy PKCS#1 encrypted key + correct
     passphrase → loads.
   - `TestWrongPassphraseFailsClearly` (AC3): wrong passphrase on legacy encrypted key → error mentions
     "could not decrypt", returns no material; assert no key/passphrase substring in the error.
   - `TestPKCS8EncryptedKeyUnsupported` (AC3 sibling): a `-----BEGIN ENCRYPTED PRIVATE KEY-----`
     block (hand-crafted PEM with that type) → clear "not supported" error, no material, no panic.
   - `TestMissingCertFileFailsNamed` (AC4): nonexistent cert path → error names the path; assert an
     audit log line was emitted with profile+path and no secret (capture via a test `slog.Handler`).
   - `TestSharedProfileReadOnce` (AC5): two endpoints' resolved profiles pointing at the same cert
     path → cert file opened exactly once (count reads via a temp file wrapper or by asserting the
     same `*Material` pointer is returned on the second `Load`).
   - `TestCABundleLoadedIntoTrustPool` (AC6): CA bundle of two CAs → `Material.TrustPool != nil` and
     contains both subjects (`pool.Subjects()` length/contents).
   - `TestEmptyCABundleFailsNamed` (AC6 negative): an unparseable CA file → named
     "no valid CA certificates in <path>" load error, no material — proves an empty/bad bundle is a
     hard failure, not a silent empty pool.
   - `TestNoSecretMaterialInLogs` (AC7): capture all logs at debug during a successful and a failed
     load → assert no cert body / private key / passphrase appears.
3. `LoadAll`: `TestLoadAllNoTLSIsNoop` (plain cfg → nil, nothing loaded) and
   `TestLoadAllAbortsOnBadProfile` (one bad cert path → error returned).

## Norms

1. **Go style:** `gofmt`/`go vet` clean. Errors are values, wrapped with `fmt.Errorf("...: %w")`,
   naming the profile (`rp.Name`) and file path — never key/cert/passphrase bytes.
2. **Boundary:** only `internal/tlsprov` imports `crypto/tls`/`crypto/x509`. Core logic (b2bua,
   config) depends on the `Provider` interface, not the library. No external dependency added.
3. **Owned state:** the cache is a single mutex-guarded map owned by one `StdProvider`; accessed
   only through `Load`. No package-level mutable state, no global singleton.
4. **Dependency injection:** the provider is passed into the engine via `WithTLSProvider` (mirrors
   `WithMetrics`); constructed at the edge (`main.go`).
5. **Security logging:** a single audit message shape — `"tls certificate load failed"` with
   `profile` + `path` fields only. Passphrase is never logged at any level.
6. **Tests (BDD, real fakes):** real cert files in `t.TempDir()`; no mocks of internal code; one
   behavior per test; `go test -race` clean for the cache.
7. **No new abstraction beyond the boundary (YAGNI):** no reload, no rotation, no pinning, no cert
   registry (deferred per requirement).

## Safeguards

1. **Functional:** `provider.Load(rp)` returns usable `Material` for a valid profile; `LoadAll`
   loads every referenced profile at startup and returns the first failure (AC1, AC4).
2. **Encrypted keys:** legacy PKCS#1 (`DEK-Info`) keys decrypt with the correct passphrase (AC2);
   wrong passphrase fails with a clear, secret-free error and yields no material (AC3). PKCS#8
   `ENCRYPTED PRIVATE KEY` is explicitly unsupported and fails with an actionable error (AC3 sibling).
3. **Fail-fast:** an unloadable certificate (missing file, bad key/cert pair, undecryptable key,
   invalid CA bundle) aborts startup before any listener binds (AC4); `main.go` exits non-zero.
4. **Load once / reuse:** a cert+key path pair is read from disk exactly once; repeated `Load`
   calls return the cached `*Material` (AC5). Performance: plain configs do zero TLS work.
5. **CA trust pool:** a valid CA bundle yields a non-empty `*x509.CertPool`; an empty/unparseable
   bundle is a named load error, not a silent empty pool (AC6).
6. **No secret leakage:** no certificate body, private key, or passphrase appears in any log line
   or returned error, at any level (AC7). Verified by `TestNoSecretMaterialInLogs`.
7. **Boundary integrity:** `internal/config` and `internal/b2bua` import no TLS library directly;
   verifiable via `go list -deps`. No import cycle (`tlsprov` never imports `b2bua`).
8. **Concurrency:** the cache is race-clean under concurrent `Load` (mutex); `go test -race` passes.
9. **Dependency:** zero new dependencies — standard library only. PKCS#8-encrypted keys are a
   documented, errored limitation, not a silent failure.
10. **Definition of done (AGENTS.md):** `go build ./...`, `go vet ./...`, `gofmt`, `go test -race ./...`
    pass; behavior tests cover AC1–AC7; only external resources (filesystem cert files) are real
    fakes, internal code tested directly.
