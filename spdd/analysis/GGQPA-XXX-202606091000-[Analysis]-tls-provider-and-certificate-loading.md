# SPDD Analysis: TLS provider boundary & certificate loading

> Source: `requirements/[User-story-13]tls-provider-and-certificate-loading.md`
> (STORY-001-013), derived from `requirements/support-sip-tls.md`. Synced two-layer model:
> the provider loads a `tls_profile`'s certificate directly (no separate `certificates` layer),
> eagerly at startup.

## Original Business Requirement

> Reproduced from `requirements/[User-story-13]tls-provider-and-certificate-loading.md`.

This story introduces the **`TLSProvider`** boundary that isolates the TLS library from the
listener, dialers, and config parsing, and delivers its first responsibility: **loading a
profile's certificate** (cert + key, optional passphrase for encrypted keys, optional CA
bundle) into usable key material and a trust pool. Loading happens at startup (fail fast on
an unloadable cert); a profile referenced by many endpoints is loaded once and reused. No
certificate or private-key material ever appears in logs.

(Full Business Value / Dependencies / Scope In / Scope Out / Acceptance Criteria AC1–AC7 /
Non-Functional Expectations as in the source story file.)

## Domain Concept Identification

### Existing Concepts (from codebase)
- **Resolved TLS profile** (`internal/config`, from `[STORY-001-012]`): the flat,
  library-agnostic value the provider consumes — supplies `cert`/`key`/`passphrase`/`ca`
  paths. The provider must depend on this, never re-parse YAML.
- **Startup wiring** (`cmd/sip-sequencer/main.go`): `config.Load` → `b2bua.New(cfg)` →
  `eng.Run`. Certificate loading is a new startup step between `Load` and serving traffic;
  a load failure must abort startup (like the existing `config.Load`/`b2bua.New` error exits
  at `main.go:29`/`main.go:49`) before any listener binds.
- **Boundary/interface idiom** (AGENTS.md + `engine.go` `MetricsSink`/`Option`): small
  consumer-defined interfaces, dependencies passed in, no global singletons. `TLSProvider`
  follows the same shape as the existing `MetricsSink` seam.
- **`slog` structured logging** (used throughout, e.g. `engine.go`): the audit-log channel;
  the no-secrets rule constrains what fields may be logged.

### New Concepts Required
- **`TLSProvider`** — the boundary interface isolating `crypto/tls` + `crypto/x509`. Methods
  (this story): load a profile's certificate → key material + trust pool. (Context-building
  methods arrive in `[STORY-001-014]`.) Default impl over the standard library; alternative
  (OpenSSL/HSM) implements the same interface.
- **Loaded certificate** — parsed `tls.Certificate` (key material) for one profile.
- **Trust pool** — `x509.CertPool` built from the profile's optional CA bundle, for later
  peer/chain verification.
- **Certificate cache** — keyed so a profile referenced by many endpoints reads files once.

### Key Business Rules
- **Eager load at startup, fail fast** (AC4): an unloadable cert aborts startup with a named
  error — satisfies the requirement's parse-time rule that cert/key "must be loadable".
- **Encrypted-key support via passphrase** (AC2, AC3): decrypt with `golang.org/x/crypto`;
  wrong/empty passphrase fails clearly, no key material produced.
- **Load once, reuse** (AC5): cache by profile (or by cert-file identity) so repeated
  references do not re-read disk.
- **CA bundle → trust pool** (AC6).
- **No secret material in logs** (AC7): only profile name, file path, outcome.
- **Narrow boundary** (NFR): core logic depends only on `TLSProvider`, never `crypto/tls`.

## Strategic Approach

### Solution Direction
- **New `internal/tls` (or `internal/b2bua/tlsprov`) package** owning the `TLSProvider`
  interface + a default `stdProvider`. Consumes resolved profiles from `internal/config`;
  exposes loaded key material + trust pool. Wired at startup in `main.go` (or inside
  `b2bua.New`), so a load error aborts before listeners bind.
- **Default impl**: `tls.LoadX509KeyPair` for unencrypted PEM; for encrypted keys, read PEM,
  `x509.DecryptPEMBlock`/`golang.org/x/crypto` to decrypt with the passphrase, then assemble
  the `tls.Certificate`. CA bundle → `x509.NewCertPool().AppendCertsFromPEM`.
- **Caching**: a mutex-guarded `map[key]*loadedCert` inside the provider; key by absolute
  cert path (+ key path) so two profiles naming the same file still read once. Single owner,
  narrow interface — aligns with AGENTS.md's "small, owned, narrow" rule for required state.
- **Data flow**: resolved profile → provider.LoadCertificate(profile) → {keyMaterial,
  trustPool} cached → consumed by `[STORY-001-014]` context builders.

### Key Design Decisions
- **Eager (startup) vs. lazy (first-use) load:** → **Eager at startup.** The synced
  requirement lists cert/key loadability under parse-time validation; eager load gives
  fail-fast-at-startup and a clean audit point, and removes mid-call surprise. (Reverses the
  prior lazy-load design.)
- **Cache key — by profile name vs. by cert-file path:** → **By cert-file path.** Honors
  "same certificate read once" even if a future second profile names the same file (and is a
  no-op for the common one-profile-one-file case). Cheap forward-compatibility with the
  deferred cert registry.
- **Encrypted-key dependency:** standard library cannot decrypt PKCS#8/encrypted PEM →
  **add `golang.org/x/crypto`** (already an indirect dep via sipgo's tree; promote to direct).
  Confirm exact decrypt path (PKCS#1 `DecryptPEMBlock` is deprecated/legacy; PKCS#8 encrypted
  needs `x/crypto/pkcs8` or equivalent) during REASONS Canvas.
- **Where the provider is constructed:** in `main.go` and passed into `b2bua.New` as an
  `Option` (mirrors `WithMetrics`), vs. constructed inside `New`. → **Pass in as an Option**
  — keeps `New` testable and the boundary swappable without touching the engine.

### Alternatives Considered
- **Load inside `internal/config`**: rejected — pulls `crypto/tls` into the pure config core,
  violating the library-agnostic boundary and AGENTS.md's pure-core rule.
- **No cache (load per use)**: rejected — violates AC5 and the performance NFR.
- **`tls.X509KeyPair` from in-memory bytes only**: fine for tests, but the source of truth is
  files on disk; keep file loading in the provider, allow byte-injection in tests (real fakes).

## Risk & Gap Analysis

### Requirement Ambiguities
- **R1 — encrypted-key format.** "Password-protected key files" — PKCS#1 (legacy
  `DEK-Info`/`ENCRYPTED`) vs. PKCS#8 encrypted? Go's `x509.DecryptPEMBlock` is deprecated and
  only handles PKCS#1; modern encrypted keys are PKCS#8. Decide the supported format(s) and
  the exact `x/crypto` entry point before implementation.
- **R2 — "load once" identity.** Cache by profile name or cert-file path? (See decision —
  recommend file path; confirm.)
- **R3 — audit-log destination/level.** AC4 says "audit log entry". Is that a distinct audit
  channel or `slog` at a defined level? Recommend `slog.Error` with a stable message + path
  field, reusing the existing logger.

### Edge Cases
- Cert present but key missing (or vice versa): named error, no partial key material.
- Cert/key mismatch (key does not match cert): `tls.LoadX509KeyPair` already errors — surface
  it named, no bytes.
- CA bundle file present but empty / unparseable: `AppendCertsFromPEM` returns false — treat
  as a load failure (named), not a silent empty pool.
- Passphrase set but key is **not** encrypted: decide ignore vs. error (recommend ignore with
  a debug note).
- Concurrent first-use of the same profile (two endpoints racing at startup): the cache mutex
  must guarantee single disk read (`go test -race`).

### Technical Risks
- **`golang.org/x/crypto` promotion + decrypt path** (R1): the one real external dependency
  decision; medium risk if the encrypted-key format is unsettled.
- **Secret leakage in errors** (AC7): wrapped errors from `crypto/*` generally do not echo key
  bytes, but a careless `%v` on a PEM block could — review every error/log site; never log the
  passphrase. Mockable only at the external boundary (filesystem); per AGENTS.md, test with
  real cert files (testdata), not mocks.
- **Cache concurrency**: single-writer map under mutex; small surface, but must be race-clean.

### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Load unencrypted cert | Yes | `tls.LoadX509KeyPair`. |
| AC2 | Decrypt encrypted key w/ passphrase | Partial | Blocked on R1 (format + `x/crypto` entry point). |
| AC3 | Wrong/missing passphrase fails clearly | Partial | Same as AC2; ensure no key material on failure. |
| AC4 | Missing file fails fast at startup, audited | Yes | Eager load in startup wiring; named error + audit log. |
| AC5 | Shared profile read once | Yes | Path-keyed cache under mutex. |
| AC6 | CA bundle → trust pool | Yes | `x509.NewCertPool` + `AppendCertsFromPEM`. |
| AC7 | No secret material in logs | Yes | Review all error/log sites; log only name/path/outcome. |

**Summary:** AC1, AC4, AC5, AC6, AC7 directly addressable with the standard library + the
existing boundary/startup idioms. AC2/AC3 blocked on **R1** (encrypted-key format + the exact
`golang.org/x/crypto` decrypt path) — settle before REASONS Canvas.
