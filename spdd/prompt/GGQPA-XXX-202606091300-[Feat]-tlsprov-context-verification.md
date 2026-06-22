# TLS Context Construction & Certificate Verification Policy (STORY-001-014)

## Requirements

Extend the `internal/tlsprov` boundary to turn a resolved `tls_profile` plus its loaded
`Material` (cert + trust pool, from STORY-001-013) into a ready-to-use `*tls.Config` — one
server-side (inbound) and one client-side (outbound) — that enforces the configured crypto and
verification policy: minimum TLS version, TLS 1.2 cipher allowlist, mutual-TLS peer
verification, allowed-subject pinning, certificate-chain depth cap, and optional date-check
relaxation. Rules without a native `tls.Config` field (subjects, depth, date relaxation) are
enforced in a verification callback. Posture: the TLS 1.2 floor and cipher allowlist always apply
and weak versions/ciphers are always rejected, but **peer-certificate verification is opt-in via
`verify_peer`**. An inbound listener requires no client cert by default; an **outbound leg is
encrypt-only by default — it accepts any server certificate** and validates the remote (chain,
dates, hostname, subjects, depth) only when `verify_peer` is set.

Boundary: build TLS contexts only. Opening listeners (015) and dialing (016) consume the
produced `*tls.Config`; this story does not bind sockets or handshake on the wire (verification
logic is unit-tested via in-process handshakes).

## Entities

```mermaid
classDiagram
direction TB

class Provider {
    <<interface>>
    +Load(rp ResolvedTLSProfile) (*Material, error)
    +ServerConfig(rp ResolvedTLSProfile) (*tls.Config, error)
    +ClientConfig(rp ResolvedTLSProfile) (*tls.Config, error)
}

class StdProvider {
    -mu sync.Mutex
    -cache map~string~*Material
    -cfgCache map~string~*tls.Config
    -log *slog.Logger
    +ServerConfig(rp ResolvedTLSProfile) (*tls.Config, error)
    +ClientConfig(rp ResolvedTLSProfile) (*tls.Config, error)
}

class Material {
    +tls.Certificate Certificate
    +*x509.CertPool TrustPool
}

class ResolvedTLSProfile {
    +TLSVersion MinVersion
    +[]string Ciphers
    +bool VerifyPeer
    +int VerifyDepth
    +bool VerifyDates
    +[]string VerifySubjects
    +...cert fields
}

Provider <|.. StdProvider : implements
StdProvider --> Material : Load (cached)
StdProvider --> ResolvedTLSProfile : reads policy
StdProvider ..> verifier : installs VerifyPeerCertificate (strict)
StdProvider ..> connVerifier : installs VerifyConnection (relaxed dates)
```

Notes:
- `Provider`, `StdProvider`, `Material` already exist (STORY-001-013). This story **adds two
  methods** to `Provider` and `StdProvider`, plus a `cfgCache` and pure mapping/verification
  helpers. No new types beyond unexported helpers. `config.ResolvedTLSProfile` is consumed as-is.
- `*tls.Config` is the boundary's artifact: the **server** config is passed to sipgo
  `ListenAndServeTLS` (015); the **client** config is installed on a per-profile UA via
  `WithUserAgenTLSConfig` (016). sipgo requires `*tls.Config`, so the interface deliberately
  exposes it — the swappable seam abstracts *loading + policy construction*, not the platform
  TLS type.

## Approach

1. **Two builder methods on the boundary:**
   - `ServerConfig(rp)` and `ClientConfig(rp)` each `Load` the profile's `Material` (cached) then
     assemble a `*tls.Config`, cached by `role+":"+rp.Name` so a profile builds once.

2. **Native fields (secure by default):**
   - `MinVersion = mapVersion(rp.MinVersion)` (`tlsv1.2`→`VersionTLS12`, `tlsv1.3`→`VersionTLS13`);
     `MaxVersion = VersionTLS13`.
   - `CipherSuites = mapCiphers(rp.Ciphers)` — names → ids restricted to **TLS 1.2** suites; `nil`
     when `rp.Ciphers` is empty (Go secure defaults). Unknown / non-1.2 name → error (this is
     where cipher-name validation lands; STORY-012 carried ciphers opaque).
   - `Certificates = []tls.Certificate{Material.Certificate}` (server cert; client cert for mTLS).

3. **Server verification (`ServerConfig`):**
   - `rp.VerifyPeer == false` → `ClientAuth = NoClientCert`; no callback (AC1: one-way handshake).
   - `rp.VerifyPeer == true` + `verify_dates == true` → `ClientAuth = RequireAndVerifyClientCert`,
     `ClientCAs = Material.TrustPool`, `VerifyPeerCertificate = verifier(rp.VerifyDepth, rp.VerifySubjects)`
     (Go validates the client chain + dates + key usage; the callback adds depth/subject) (AC2, AC3, AC4, AC7).
   - `rp.VerifyPeer == true` + `verify_dates == false` → `ClientAuth = RequireAnyClientCert` (Go's
     RequireAndVerify always enforces dates and `InsecureSkipVerify` is client-only, so require a cert
     but verify it ourselves), `VerifyConnection = connVerifier(TrustPool, rp.VerifyDepth, rp.VerifySubjects,
     ExtKeyUsageClientAuth, checkHostname=false)` — a client certificate has no hostname to match (AC7).

4. **Client verification (`ClientConfig`):**
   - **Relaxed by default (`verify_peer == false`):** the outbound leg is encrypt-only — set
     `InsecureSkipVerify = true` and install no verification callback. Any server certificate is
     accepted (self-signed, expired, hostname mismatch, untrusted CA). Strict validation is opt-in.
   - **`verify_peer == true` + `verify_dates == true`:** leave `InsecureSkipVerify = false` so Go
     performs its standard chain + date + **hostname** (`ServerName`) + key-usage verification against
     `RootCAs = Material.TrustPool` (nil → system roots); install `VerifyPeerCertificate =
     verifier(depth, subjects)` to add the depth cap and subject allowlist.
   - **`verify_peer == true` + `verify_dates == false`:** set `InsecureSkipVerify = true` and install
     `VerifyConnection = connVerifier(RootCAs, depth, subjects, ExtKeyUsageServerAuth, checkHostname=true)`
     — it re-validates the full chain (incl. key usage) with the date window relaxed but **still
     enforces the peer hostname** from `cs.ServerName`. Relaxing dates never relaxes identity.

5. **Verification callbacks (the security-sensitive core):**
   - **`verifier` (strict, `VerifyPeerCertificate`):** used when validation is on and dates are
     checked. Go has already done full chain + date + key-usage (+ client-side hostname) verification
     and passes `verifiedChains`; the callback only **adds** the depth cap and subject allowlist via
     the shared `checkChains` helper. Go's vetted verification is never bypassed.
   - **`connVerifier` (relaxed dates, `VerifyConnection`):** used when validation is on but
     `verify_dates:false`. Go's built-in verification is off (`InsecureSkipVerify` on the client /
     `RequireAnyClientCert` on the server), so it rebuilds the chain from the presented certs:
     `leaf.Verify(VerifyOptions{Roots, Intermediates, KeyUsages:[ku], CurrentTime: leaf.NotBefore,
     DNSName: cs.ServerName when checkHostname})`. `CurrentTime = leaf.NotBefore` neutralizes only the
     date window while keeping chain + key-usage + (client) hostname checks; then `checkChains` applies
     depth + subject. `VerifyConnection` (not `VerifyPeerCertificate`) is required because only it
     carries the negotiated `ServerName`.

6. **Pure helpers** (`mapVersion`, `mapCiphers`, `checkDepth`, `checkSubject`) are pure functions,
   unit-tested directly (AGENTS.md). No GlobalExceptionHandler / no layering — errors are values.

## Structure

### Type/Interface Relationships
1. `Provider` gains `ServerConfig`/`ClientConfig`; `StdProvider` implements them. `Material`,
   `NewStdProvider` unchanged.
2. `verifier(maxIntermediates int, subjects []string) func([][]byte, [][]*x509.Certificate) error`
   returns the strict `VerifyPeerCertificate` callback; `connVerifier(roots, maxIntermediates,
   subjects, ku, checkHostname bool) func(tls.ConnectionState) error` returns the relaxed-dates
   `VerifyConnection` callback. Both delegate depth + subject to the shared `checkChains` helper.
3. `mapVersion`, `mapCiphers`, `checkDepth`, `checkSubject` are unexported pure helpers.
4. No new package; all in `internal/tlsprov`. Still the only importer of `crypto/tls`/`crypto/x509`.

### Dependencies
1. `StdProvider.ServerConfig`/`ClientConfig` → `Load` (cache) → build `*tls.Config` → `cfgCache`.
2. `verifier` (strict) reads `verifiedChains`; `connVerifier` (relaxed) calls `leaf.Verify` to
   rebuild; both → `checkChains` → `checkDepth`, `checkSubject`. `ServerConfig`/`ClientConfig` share
   `cached` (cache get/build/store) and `loadAndBase` (cert + version-floor + cipher base config).
3. `mapCiphers` reads `tls.CipherSuites()` to build the name→id allowlist for TLS 1.2.
4. Consumers (b2bua listener/dialer, 015/016) call these via the stored `tlsprov.Provider`.

### Layering
1. Boundary layer: `Provider` (Load + ServerConfig + ClientConfig).
2. Build layer: assemble `*tls.Config` from `Material` + policy; cache per role+profile.
3. Verification layer: `verifier` factory + pure `checkDepth`/`checkSubject`.
4. Mapping layer: pure `mapVersion`/`mapCiphers`.

## Operations

### Extend interface — `internal/tlsprov/provider.go`
1. Add to `Provider`: `ServerConfig(rp config.ResolvedTLSProfile) (*tls.Config, error)` and
   `ClientConfig(rp config.ResolvedTLSProfile) (*tls.Config, error)`.
2. Add `cfgCache map[string]*tls.Config` to `StdProvider`; initialize in `NewStdProvider`.

### Implement `mapVersion` / `mapCiphers` — `internal/tlsprov/policy.go`
1. `func mapVersion(v config.TLSVersion) (uint16, error)`: `TLSv12`→`tls.VersionTLS12`,
   `TLSv13`→`tls.VersionTLS13`; empty → `tls.VersionTLS12` (secure default); else error
   `"unsupported min_version %q"`.
2. `func mapCiphers(names []string) ([]uint16, error)`: if `len(names)==0` return `nil`. Build a
   `map[string]uint16` once from `tls.CipherSuites()` keeping only suites whose `SupportedVersions`
   includes `tls.VersionTLS12`. For each name: lookup; unknown / not-1.2 → error
   `"unknown or non-TLS1.2 cipher %q"`. Return the id slice (order preserved).

### Implement `checkDepth` / `checkSubject` — `internal/tlsprov/policy.go`
1. `func checkDepth(chain []*x509.Certificate, maxIntermediates int) error`: `intermediates :=
   len(chain) - 2` (chain = leaf … root); if `intermediates < 0 { intermediates = 0 }`; if
   `intermediates > maxIntermediates` → error `"certificate chain too long: %d intermediates exceeds verify_depth %d"`.
   (OpenSSL semantics: `verify_depth` = max intermediate CAs between leaf and trust anchor.)
2. `func checkSubject(leaf *x509.Certificate, allow []string) error`: if `len(allow)==0` return nil;
   if `leaf.Subject.String()` is in `allow` return nil; else error `"peer subject %q not in verify_subjects"`.
   Exact string match on the leaf subject (SAN matching is out of scope).

### Implement verification callbacks — `internal/tlsprov/verify.go`
1. `func verifier(maxIntermediates int, subjects []string) func([][]byte, [][]*x509.Certificate) error`
   — strict `VerifyPeerCertificate`: Go has already validated chain + dates + key usage (+ client
   hostname); the callback just returns `checkChains(verifiedChains, maxIntermediates, subjects)`.
2. `func connVerifier(roots *x509.CertPool, maxIntermediates int, subjects []string,
   ku x509.ExtKeyUsage, checkHostname bool) func(tls.ConnectionState) error` — relaxed-dates
   `VerifyConnection`: parse `cs.PeerCertificates` → leaf + intermediates; `roots == nil` →
   `x509.SystemCertPool()`; `leaf.Verify(VerifyOptions{Roots, Intermediates, KeyUsages:[ku],
   CurrentTime: leaf.NotBefore, DNSName: cs.ServerName when checkHostname})`; on err → wrapped chain
   error; then `checkChains(built, ...)`.
3. `func checkChains(chains [][]*x509.Certificate, maxIntermediates int, subjects []string) error`
   — shared: accept if any chain passes both `checkDepth` and `checkSubject`; else the first failure
   (or "no verified certificate chain").
4. Constraints: never log certificate bytes; callbacks return errors only (handshake aborts).

### Implement `ServerConfig` — `internal/tlsprov/context.go`
1. `func (p *StdProvider) ServerConfig(rp config.ResolvedTLSProfile) (*tls.Config, error)`.
2. Logic:
   - `key := "server:" + rp.Name`; under `p.mu`, return cached `*tls.Config` if present.
   - `m, err := p.Load(rp)` (cached cert material).
   - `min, err := mapVersion(rp.MinVersion)`; `suites, err := mapCiphers(rp.Ciphers)`.
   - Base: `&tls.Config{Certificates: []tls.Certificate{m.Certificate}, MinVersion: min,
     MaxVersion: tls.VersionTLS13, CipherSuites: suites}`.
   - If `rp.VerifyPeer`: require `m.TrustPool != nil` else error
     `"tls_profiles[%q]: verify_peer requires a ca bundle"`; set `ClientCAs = m.TrustPool`. Then:
     - `verify_dates` true → `ClientAuth = tls.RequireAndVerifyClientCert`,
       `VerifyPeerCertificate = verifier(rp.VerifyDepth, rp.VerifySubjects)`.
     - `verify_dates` false → `ClientAuth = tls.RequireAnyClientCert` (Go's RequireAndVerify always
       enforces dates and `InsecureSkipVerify` is client-only, so require a cert but verify it
       ourselves), `VerifyConnection = connVerifier(m.TrustPool, rp.VerifyDepth, rp.VerifySubjects,
       x509.ExtKeyUsageClientAuth, false)` (a client cert has no hostname to match).
   - Else: `ClientAuth = tls.NoClientCert` (no callback).
   - Cache and return. (The cache get/store is `cached`; the base config is `loadAndBase`.)

### Implement `ClientConfig` — `internal/tlsprov/context.go`
1. `func (p *StdProvider) ClientConfig(rp config.ResolvedTLSProfile) (*tls.Config, error)`.
2. Logic:
   - `key := "client:" + rp.Name`; cache check under `p.mu`.
   - `m, err := p.Load(rp)`; `min`/`suites` as above.
   - Base: `&tls.Config{Certificates: []tls.Certificate{m.Certificate}, MinVersion: min,
     MaxVersion: tls.VersionTLS13, CipherSuites: suites, RootCAs: m.TrustPool}` (nil RootCAs → Go
     uses system roots).
   - Switch on policy:
     - `!rp.VerifyPeer` (default) → `InsecureSkipVerify = true`, no callback: encrypt-only, accept
       any server cert.
     - `rp.VerifyDates` → `VerifyPeerCertificate = verifier(rp.VerifyDepth, rp.VerifySubjects)`
       (Go validates chain + dates + hostname; callback adds depth/subject).
     - else → `InsecureSkipVerify = true` + `VerifyConnection = connVerifier(m.TrustPool,
       rp.VerifyDepth, rp.VerifySubjects, x509.ExtKeyUsageServerAuth, true)` (relax dates, keep chain
       + key usage + hostname).
   - Cache and return. (Shared `cached` + `loadAndBase` as in `ServerConfig`.)

### Add tests — `internal/tlsprov/context_test.go`
1. BDD, real certs in `t.TempDir()`: a test CA mints a leaf (and an intermediate for chain-depth
   cases), an expired leaf, and a wrong-subject leaf — all via stdlib `x509.CreateCertificate`.
   Drive handshakes in-process (`tls.Server`/`tls.Client` over `net.Pipe`, or a localhost
   listener) — no mocks; external peer is the only boundary (AGENTS.md).
2. Cover AC1–AC8:
   - `TestDefaultServerOneWayHandshake` (AC1): default profile → client with no cert handshakes OK.
   - `TestVerifyPeerEnforcesMTLS` (AC2): `verify_peer:true` → no-cert client rejected; CA-signed
     client accepted.
   - `TestVerifySubjectsRestrictsPeers` (AC3): allowlist `[CN=phone.internal]` → `CN=other` rejected,
     `CN=phone.internal` accepted.
   - `TestVerifyDepthCapsChain` (AC4): `verify_depth:0` → chain with one intermediate rejected;
     `verify_depth:1` → accepted. (Pins the OpenSSL intermediate-count semantics.)
   - `TestMinVersionEnforced` (AC5): server `min_version:tlsv1.2` → TLS 1.1 client handshake fails.
   - `TestCiphersTLS12Only` (AC6): allowlist one 1.2 suite → 1.2 handshake only that suite; a 1.3
     handshake ignores the allowlist (Go fixed suites). Unknown cipher name → `ClientConfig`/`ServerConfig` error.
   - `TestVerifyDatesToggle` (AC7): expired remote rejected with `verify_dates:true`, accepted with
     `verify_dates:false`; assert chain still validated (untrusted-CA expired cert still rejected under false).
   - `TestClientValidatesAgainstCA` (AC8): with `verify_peer:true`, remote signed by a CA not in the
     bundle rejected; in-bundle accepted. (Client validation tests now set `verify_peer:true`, since
     validation is opt-in.)
   - `TestRelaxedDefaultAcceptsAnyServerCert`: `verify_peer:false` (default) accepts a self-signed,
     expired, wrong-hostname server cert (encrypt-only).
   - `TestVerifyDatesFalseStillChecksHostname`: `verify_peer:true` + `verify_dates:false` rejects a
     wrong-hostname cert (MITM guard) yet accepts an expired right-hostname cert.
   - `TestServerVerifyDatesFalseAcceptsExpiredClientCert`: inbound `verify_peer:true` +
     `verify_dates:false` accepts an expired CA-trusted client cert, still rejects an untrusted-CA one.
   - Negatives: `TestVerifyPeerWithoutCABundleErrors`; `TestUnknownCipherErrors`; `TestUnsupportedMinVersionErrors`.

## Norms

1. **Go style:** `gofmt`/`go vet` clean. Errors are values, wrapped with `fmt.Errorf("...: %w")`,
   naming the profile (`rp.Name`); never log certificate/key bytes.
2. **Boundary:** only `internal/tlsprov` imports `crypto/tls`/`crypto/x509`. The `Provider`
   interface exposes `*tls.Config` deliberately (sipgo requires it); no other library leaks.
3. **Relaxed-by-default, strict-on-demand:** `verify_peer:false` (default) is encrypt-only on the
   outbound leg — `InsecureSkipVerify = true` with no callback (a deliberate posture, not an
   oversight). When `verify_peer:true` + `verify_dates:true`, Go's native verification runs unmodified
   and `verifier` only *adds* checks. `InsecureSkipVerify` is otherwise paired with a chain-rebuilding
   `connVerifier` (relaxed dates) that re-validates chain, key usage, and (client) hostname — never
   bare except in the explicit encrypt-only default.
4. **Pure helpers:** `mapVersion`/`mapCiphers`/`checkDepth`/`checkSubject` are pure and table-tested.
5. **Build once:** contexts cached per `role+profile` (pairs with the cert cache); read-only after build.
6. **Tests (BDD, real fakes):** real generated certs; in-process handshakes; one behavior per test;
   `go test -race` clean.
7. **No new abstraction (YAGNI):** no opaque wrapper type around `*tls.Config`; no per-call rebuild;
   no SAN matching, no CRL/OCSP (not in scope).

## Safeguards

1. **Default posture (operator-owned):** a profile with all policy omitted yields a TLS 1.2 floor and
   the cipher allowlist, and **no peer-certificate verification** — an inbound listener demands no
   client cert and an outbound leg is encrypt-only (accepts any server cert). This is a deliberate,
   documented choice; strict peer validation is opt-in via `verify_peer:true` (AC1, AC2).
2. **mTLS:** `verify_peer:true` requires and verifies the client certificate against the trust pool;
   a missing trust pool is a build-time error, not a silent accept (AC2).
3. **Subject pinning:** with a non-empty `verify_subjects`, only a leaf whose exact subject string is
   listed is accepted (AC3).
4. **Chain-depth cap:** chains with more intermediates than `verify_depth` are rejected; default 2 (AC4).
5. **Version floor:** handshakes below `min_version` are rejected; `MaxVersion` pinned to TLS 1.3 (AC5).
6. **Cipher control:** the `ciphers` allowlist applies to TLS 1.2 only and is ignored on TLS 1.3;
   unknown / non-1.2 names fail context construction with a naming error (AC6).
7. **Date relaxation is bounded:** under `verify_peer:true`, `verify_dates:false` relaxes only the
   date window — chain validation against the configured roots, key usage, depth, subjects, and (on
   the client) the peer **hostname** still apply (AC7). Relaxing dates never relaxes identity.
8. **Client CA validation:** the client rejects a remote whose chain does not terminate in the
   configured CA bundle (or system roots when none configured) (AC8).
9. **No secret leakage:** no certificate/key material in errors or logs; verification failures abort
   the handshake and surface a sanitized reason.
10. **Boundary integrity / concurrency:** `internal/config` imports no TLS library; contexts built
    once and cached under the existing mutex; `go test -race` passes.
11. **Definition of done (AGENTS.md):** `go build ./...`, `go vet ./...`, `gofmt`, `go test -race ./...`
    pass; behavior tests cover AC1–AC8 incl. the relaxed-but-still-validated date path.
