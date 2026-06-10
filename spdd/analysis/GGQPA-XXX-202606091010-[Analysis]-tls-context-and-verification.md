# SPDD Analysis: TLS context construction & certificate verification policy

> Source: `requirements/[User-story-14]tls-context-and-verification.md` (STORY-001-014),
> derived from `requirements/support-sip-tls.md`. Synced two-layer model; the provider turns
> a resolved `tls_profile` into server- and client-side TLS contexts enforcing the configured
> crypto + verification policy.

## Original Business Requirement

> Reproduced from `requirements/[User-story-14]tls-context-and-verification.md`.

The provider must turn a resolved profile into a ready-to-use TLS context — one server-side
(inbound), one client-side (outbound) — enforcing certificate, minimum version, TLS 1.2
cipher allowlist, and peer verification (require client cert, restrict subjects, cap chain
depth, optionally relax date checks). Several rules have no direct standard-library field and
are enforced in a verification callback. Secure-by-default: omitted policy yields safe
behavior; weak versions/ciphers are rejected.

(Full Business Value / Dependencies / Scope In / Scope Out / Acceptance Criteria AC1–AC8 /
Non-Functional Expectations as in the source story file.)

## Domain Concept Identification

### Existing Concepts (from codebase)
- **`TLSProvider`** (from `[STORY-001-013]`): the boundary; this story adds its
  context-building responsibility (server context, client context) over the already-loaded
  key material + trust pool. No new package — extends the same provider.
- **Resolved TLS profile** (`internal/config`): supplies `min_version`, `ciphers`,
  `verify_peer`, `verify_depth`, `verify_dates`, `verify_subjects` — all consumed here.
- **Consumers**: the inbound listener (`[STORY-001-015]`) needs the server context; the
  outbound dialer (`[STORY-001-016]`, at `bridge.go` `runAppChain`/`dialPBX`) needs the
  client context. This story produces what both consume — a `*tls.Config` (default impl)
  behind the boundary.

### New Concepts Required
- **Server-side TLS context** — from an inbound profile: certificate, `MinVersion`, TLS 1.2
  `CipherSuites`, and `ClientAuth` + a `VerifyPeerCertificate` callback enforcing
  `verify_subjects` / `verify_depth` / `verify_dates`.
- **Client-side TLS context** — from an outbound profile: certificate (for mTLS),
  version/cipher policy, `RootCAs` = trust pool, and chain/date validation honoring
  `verify_depth` / `verify_dates`.
- **Verification callback** — the custom path for the rules with no native `tls.Config`
  field: subject allowlist, chain-depth cap, and date relaxation (`verify_dates: false`).
- **Version/cipher mapping** — `tlsv1.2`/`tlsv1.3` → `tls.VersionTLS12`/`13`; cipher constant
  names → Go `tls.CipherSuite` ids (TLS 1.2 only; 1.3 suites fixed).

### Key Business Rules
- **Secure default context** (AC1, NFR): omit-all → TLS 1.2 floor, Go default ciphers, dates
  checked, no client-cert demand.
- **`verify_peer` ⇒ mTLS** (AC2): `RequireAndVerifyClientCert`.
- **`verify_subjects` allowlist** (AC3) and **`verify_depth` cap** (AC4): enforced in the
  callback (no native field).
- **`min_version` floor enforced** (AC5).
- **Cipher allowlist = TLS 1.2 only; ignored on 1.3** (AC6).
- **`verify_dates` toggles expiry checking** (AC7) — custom path when `false`.
- **Client validates remote against configured CA** (AC8).

## Strategic Approach

### Solution Direction
- **Add two methods to `TLSProvider`**: `ServerContext(resolvedProfile)` and
  `ClientContext(resolvedProfile)`, each returning the library-specific context (default:
  `*tls.Config`) behind an opaque type so listeners/dialers stay library-agnostic.
- **Default impl** builds `*tls.Config`: set `Certificates`, `MinVersion`, `MaxVersion`
  (`tls.VersionTLS13`), `CipherSuites` (TLS 1.2 allowlist or omit for Go defaults), `RootCAs`
  (client) / `ClientCAs` + `ClientAuth` (server). The non-native rules go into
  `VerifyPeerCertificate` (server) and, for `verify_dates: false`, `InsecureSkipVerify: true`
  **with** a custom `VerifyPeerCertificate` that re-implements chain verification minus the
  date check (so disabling dates does not disable chain validation).
- **Pure mapping helpers** (version string → const, cipher name → id, subject match,
  depth check) live as pure functions — directly unit-testable per AGENTS.md; the
  context-building edge wires them into `tls.Config`.

### Key Design Decisions
- **`verify_dates: false` implementation** (the pivotal one): Go's `tls.Config` has no "skip
  only dates" switch. Options: (a) `InsecureSkipVerify: true` + a full custom
  `VerifyPeerCertificate` that builds the chain with `x509.VerifyOptions{CurrentTime: ...}`
  bypassed for dates; (b) verify normally and swallow only `x509.CertificateInvalidError`
  with `Reason == Expired`. → **Recommend (a)** — explicit, keeps all verification in one
  callback, and is the only way to also honor `verify_depth`/`verify_subjects` uniformly.
  This is a security-sensitive path: default (`verify_dates: true`) must use Go's standard
  verification untouched; the custom path is reachable only when explicitly opted in.
- **`verify_depth` semantics**: max chain length to the trusted root. Enforce by inspecting
  the built `verifiedChains` length in the callback and rejecting overlong chains. Confirm
  whether "depth 2" counts the leaf, intermediates, or root during REASONS Canvas (off-by-one
  risk).
- **Cipher allowlist scope**: set `CipherSuites` (1.2 only); document that Go ignores it for
  1.3. If `ciphers` omitted → leave `CipherSuites` nil (Go secure defaults).
- **Opaque context type at the boundary**: return a provider-defined wrapper, not a raw
  `*tls.Config`, so an OpenSSL/HSM impl can return its own structure (NFR).
- **Confirmed consumption (sipgo v1.4.0)**: the default impl's `*tls.Config` is consumed two
  ways — the **server** context is passed directly to `Server.ListenAndServeTLS(ctx,"tls",addr,
  conf)` (`[STORY-001-015]`); the **client** context is installed on a per-profile UA via
  `WithUserAgenTLSConfig` (`[STORY-001-016]`). SNI `ServerName` is auto-filled per dial host by
  sipgo's transport (`transport_tls.go`), so the client context need not set it. This confirms
  one flat `*tls.Config` per profile serves both shapes; no `ServerName` pinning needed here.

### Alternatives Considered
- **`InsecureSkipVerify` always + fully custom verification**: rejected for the default path —
  reimplementing Go's verification is risky; only use the custom path where a native field is
  insufficient (`verify_dates:false`, subjects, depth).
- **Per-call context construction**: rejected — build once per profile (performance NFR;
  pairs with the cert cache).
- **Restricting TLS 1.3 ciphers**: not possible in Go and out of scope — documented, ignored.

## Risk & Gap Analysis

### Requirement Ambiguities
- **R1 — `verify_depth` counting convention.** Default `2` — does it include leaf and/or
  root? Must be pinned to make AC4 deterministic.
- **R2 — `verify_subjects` match semantics.** Exact full-subject string match, or
  CN/SAN-aware? Examples use `CN=phone.internal`. Recommend exact match on the certificate
  subject string initially; flag SAN handling.
- **R3 — interaction `verify_dates:false` + `verify_peer:false`** (client side with no CA?):
  define behavior when relaxation is set but base verification is already off.

### Edge Cases
- `min_version` set to `tlsv1.3`: valid contiguous floor (1.3..1.3) — cipher allowlist then
  fully ignored.
- Empty `verify_subjects` with `verify_peer:true`: any valid client cert accepted (AC2 vs AC3).
- Client context with no CA bundle (`ca` omitted): falls back to system roots, or fails closed?
  Decide — recommend system roots unless `verify_peer`/pinning requires otherwise.
- Cipher allowlist names that are valid but TLS 1.3-only / unknown: tie to story-012 R2
  (validate at load) so they never reach here unchecked.

### Technical Risks
- **Custom verification correctness (security-critical)**: the `verify_dates:false` /
  depth / subjects callback must not accidentally weaken chain validation. Mitigation:
  isolate as pure, exhaustively table-tested functions (real certs in testdata, per AGENTS.md;
  external peers are the only mockable boundary).
- **Go version-specific cipher/version constants**: pin against go 1.23.6's `crypto/tls`.
- **No concurrency concern**: contexts built once at startup, read-only thereafter.

### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Default server context one-way handshake | Yes | Plain `tls.Config` with cert + 1.2 floor. |
| AC2 | `verify_peer` ⇒ mTLS | Yes | `ClientAuth = RequireAndVerifyClientCert`. |
| AC3 | `verify_subjects` restricts peers | Yes | Verification callback; pin match semantics (R2). |
| AC4 | `verify_depth` caps chain | Partial | Callback inspects chain length; counting convention (R1). |
| AC5 | Min version enforced | Yes | `MinVersion`. |
| AC6 | Ciphers 1.2 only, ignored on 1.3 | Yes | `CipherSuites` set; Go ignores on 1.3. |
| AC7 | `verify_dates` toggles expiry | Partial | Custom callback for `false` (R3); default uses native verify. |
| AC8 | Client validates vs configured CA | Yes | `RootCAs` = trust pool. |

**Summary:** AC1, AC2, AC5, AC6, AC8 directly addressable via `tls.Config` fields. AC3, AC4,
AC7 require the custom verification callback — the security-sensitive core; resolve R1–R3
(depth counting, subject match, relaxation interactions) before REASONS Canvas, and keep the
default (strict) path on Go's untouched verification.
