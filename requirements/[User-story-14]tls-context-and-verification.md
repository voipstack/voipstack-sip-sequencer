# Story Decomposition: SIP TLS transport support (story 3/5)

> Part of the TLS decomposition. Module overview and INVEST analysis in
> `[User-story-12]tls-configuration.md`. Derived from `requirements/support-sip-tls.md`.

---

## [STORY-001-014] TLS context construction & certificate verification policy

### Background
With configuration resolved (`[STORY-001-012]`) and certificates loadable through the
provider boundary (`[STORY-001-013]`), the provider must turn a resolved profile into a
ready-to-use TLS context — one for the server side (inbound) and one for the client side
(outbound) — that enforces the configured crypto and verification policy.

Server contexts carry the certificate, the minimum TLS version, the TLS 1.2 cipher
allowlist, and the peer-verification policy (require a client cert, restrict allowed
subjects, cap chain depth, optionally relax date checking). Client contexts carry the
certificate (for mTLS), the version/cipher policy, and validate the remote's chain and
dates against the configured CA. Several of these rules have no direct standard-library
field and are enforced inside a verification callback. Secure-by-default is the rule:
omitted policy yields safe behavior, and weak versions/ciphers are rejected.

Key points:
- Business value: TLS sessions enforce exactly the policy the operator declared —
  mutual auth, allowed peers, version floor — without per-call code.
- Bridges the provider to the listeners (`[STORY-001-015]`) and dialers (`[STORY-001-016]`),
  which consume these contexts directly.
- Needed now because handshakes cannot happen without a policy-bearing context.

### Business Value
- Provide enforced mutual TLS: only clients presenting an allowed, valid certificate are
  accepted.
- Support a configurable security floor — reject TLS versions and cipher suites the
  operator considers weak, secure by default.
- Enable controlled relaxation for dev/testing (e.g. disabling date checks) without
  weakening production defaults.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-012]` (resolved profile), `[STORY-001-013]` (loaded
  certificate + trust pool via the provider).
- **Data assumptions:** A resolved profile and its loaded certificate/CA are available;
  cipher names use Go constant names (not OpenSSL strings).
- **Integration points:** `crypto/tls`, `crypto/x509` in the default provider.
- **Business constraints:** Contiguous version range only (min `tlsv1.2` .. max
  `tlsv1.3`). TLS 1.3 cipher suites are fixed by Go and not configurable; the `ciphers`
  allowlist applies to TLS 1.2 only.

### Scope In
- Build a **server-side** TLS context from a resolved inbound profile: certificate,
  `min_version`, TLS 1.2 `ciphers` allowlist, `verify_peer` (require & verify client
  cert), `verify_subjects` (allowed leaf subjects), `verify_depth` (max chain length),
  and `verify_dates`.
- Build a **client-side** TLS context from a resolved outbound profile: certificate (for
  mTLS), `min_version`, `ciphers`, and validation of the remote's chain and dates against
  the configured CA, honouring `verify_depth` / `verify_dates`.
- Enforce the non-standard-library rules (`verify_depth`, `verify_subjects`, `verify_dates
  = false`) inside the peer-verification callback.
- Reject weak TLS versions (below the configured floor) and apply the cipher allowlist by
  default; omitted policy yields secure defaults.

### Scope Out
- Binding listeners / accepting connections — `[STORY-001-015]`.
- Dialing outbound / connect timeout / SRV / pooling — `[STORY-001-016]`.
- Loading certificates from disk — `[STORY-001-013]`.
- Outbound connect timeout (`connect_timeout`) — owned by the dialer (`[STORY-001-016]`);
  it is a connection-layer deadline, not part of the TLS context.

### Acceptance Criteria

#### AC1: Default server context completes a one-way TLS handshake
**Given** a server context built from an inbound profile with all policy omitted (cert
`server`, defaults: min `tlsv1.2`, no peer verification)
**When** a TLS client with no client certificate handshakes against it
**Then** the handshake completes successfully (server authenticates to the client; no
client certificate is demanded).

#### AC2: `verify_peer: true` enforces mutual TLS
**Given** a server context built from a profile with `verify_peer: true`
**When** a client presents no certificate **and** when another client presents a valid
certificate signed by the configured CA
**Then** the certificate-less client is rejected at handshake, and the client with a
valid certificate is accepted.

#### AC3: `verify_subjects` restricts which peers are accepted
**Given** a server context with `verify_peer: true` and `verify_subjects: [CN=phone.internal]`
**When** a client presents a valid certificate whose subject is `CN=other.internal`
**and** when a client presents a valid certificate whose subject is `CN=phone.internal`
**Then** the `CN=other.internal` client is rejected and the `CN=phone.internal` client is accepted.

#### AC4: `verify_depth` caps the certificate chain length
**Given** a context with `verify_depth: 2`
**When** a peer presents a certificate whose chain to the trusted root is longer than 2
**Then** verification fails and the peer is rejected.

#### AC5: Minimum version is enforced
**Given** a context with `min_version: tlsv1.2`
**When** a peer offers only TLS 1.1 (or lower)
**Then** the handshake is rejected.

#### AC6: Cipher allowlist applies to TLS 1.2 and is ignored on TLS 1.3
**Given** a context whose `ciphers` allowlist contains only
`TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256`
**When** a TLS 1.2 handshake negotiates ciphers **and** when a TLS 1.3 handshake occurs
**Then** the TLS 1.2 handshake only succeeds using a cipher from the allowlist (others
rejected), while the TLS 1.3 handshake proceeds using Go's fixed 1.3 suites regardless of
the allowlist.

#### AC7: `verify_dates` controls expiry checking
**Given** a client context validating a remote whose certificate is expired
**When** the profile has `verify_dates: true` (default) **and** when it has `verify_dates: false`
**Then** the expired certificate is rejected under `true` and accepted under `false`
(the relaxation intended for dev/testing).

#### AC8: Client context validates the remote against the configured CA
**Given** a client context whose configured CA bundle does not include the CA that signed
the remote's certificate
**When** the outbound handshake occurs
**Then** verification fails and the connection is rejected; a remote signed by a CA in the
bundle is accepted.

#### Non-Functional Expectations
- Omitting all policy fields must yield a secure context (TLS 1.2 floor, Go default
  ciphers, dates checked) — security must not depend on the operator setting fields.
- Verification-failure reasons must be loggable without exposing certificate contents.
