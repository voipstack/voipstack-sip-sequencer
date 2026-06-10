# Story Decomposition: SIP TLS transport support (story 2/5)

> Part of the TLS decomposition. Module overview and INVEST analysis in
> `[User-story-12]tls-configuration.md`. Derived from `requirements/support-sip-tls.md`.

---

## [STORY-001-013] TLS provider boundary & certificate loading

### Background
The sequencer must not be welded to one TLS library. This story introduces a single
boundary — the `TLSProvider` — that isolates "how to do TLS" from the listener, dialers,
and config parsing, so an alternative implementation (OpenSSL bindings, an HSM-backed
provider) can be substituted later with no change to core logic. The default provider
wraps Go's `crypto/tls` + `crypto/x509`.

This story delivers the boundary and its first responsibility: **loading a profile's
certificate**. Given a resolved `tls_profile` (cert + key files, optional passphrase for
encrypted keys, optional CA bundle), the provider produces usable key material and a trust
pool. Loading happens at startup so an unloadable certificate fails fast before any traffic
is served, and a profile referenced by many endpoints is loaded once and reused. No
certificate or private-key material ever appears in logs.

Key points:
- Business value: a swappable TLS implementation protects the operator's investment and
  enables hardware-backed keys later without touching call logic.
- Foundation for `[STORY-001-014]` (contexts), `[STORY-001-015]` (listener),
  `[STORY-001-016]` (dialers) — all obtain key material through this boundary.
- Satisfies the requirement's parse-time rule that a profile's `cert` / `key` must be
  loadable (incl. `passphrase` if encrypted).

### Business Value
- Provide a stable seam so the TLS library can be replaced (OpenSSL/HSM) without rewriting
  the listener, dialers, or config parsing.
- Support operators who protect private keys with a passphrase (encrypted key files).
- Enable efficient operation — a profile's certificate is read from disk once and reused
  across every endpoint that names the profile.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-012]` TLS configuration (supplies resolved profiles with
  cert/key paths and optional passphrase/CA).
- **Data assumptions:** Certificate, key, and CA files exist and are readable at startup;
  encrypted keys carry a matching passphrase on the profile.
- **Integration points:** Local filesystem (cert/key/CA files). Encrypted keys are decrypted with
  the **standard library only** (`x509.DecryptPEMBlock`, legacy PKCS#1 `DEK-Info`); modern
  PKCS#8-encrypted keys are unsupported (stdlib cannot decrypt them) and fail with a clear error.
- **Business constraints:** No certificate or private-key data in any log, at any level.
  Certificate content is never echoed on error.

### Scope In
- Define the `TLSProvider` boundary that isolates the TLS library; provide a default
  implementation over `crypto/tls` + `crypto/x509`.
- Load a profile's certificate: read cert + key PEM into usable key material.
- Decrypt password-protected key files using the profile's `passphrase` — standard library only
  (legacy PKCS#1 `DEK-Info`); PKCS#8-encrypted keys are an explicit, errored limitation.
- Load the optional CA bundle into a trust pool for later peer/chain verification.
- Load each profile's certificate once at startup and cache the parsed result so repeated
  references do not re-read files; an unloadable certificate fails startup.
- Emit an audit log entry on certificate load failure that names the file path but never
  includes certificate or key bytes.

### Scope Out
- Building `tls.Config` / server & client contexts and verification policy — `[STORY-001-014]`.
- Opening the listener / dialing connections — `[STORY-001-015]`, `[STORY-001-016]`.
- Verifying a *peer's* certificate (chain/subject/date) — context/handshake work in `[STORY-001-014]`.
- Certificate reload / rotation without restart (future consideration).
- A separate shared-certificate registry (two profiles sharing one cert file with different
  policy) — deferred until actually needed, per the requirement's Model note.

### Acceptance Criteria

#### AC1: Load an unencrypted certificate
**Given** a profile `inbound` whose `cert` and `key` point to valid, unencrypted PEM files
**When** the provider loads profile `inbound`
**Then** the certificate loads successfully and is available as usable key material for a
TLS handshake.

#### AC2: Decrypt an encrypted key with the correct passphrase
**Given** a profile `outbound` whose key file is password-protected and whose `passphrase`
is the correct password
**When** the provider loads profile `outbound`
**Then** the key is decrypted and the certificate loads successfully.

#### AC3: Wrong or missing passphrase fails clearly
**Given** a profile whose key file is password-protected and whose `passphrase` is empty or
incorrect
**When** the provider attempts to load it
**Then** loading fails with a clear error indicating the key could not be decrypted, and no
key material is produced.

#### AC4: Missing certificate file fails fast at startup, named and audited
**Given** a profile whose `cert` path does not exist on disk
**When** the provider loads it at startup
**Then** startup fails with a clear error naming the missing path, an audit log entry records
the failure, and no traffic is served.

#### AC5: A profile referenced by many endpoints is read once
**Given** a profile `outbound` referenced by both a `sequence` item and the `next_hop`
**When** the provider loads the configuration
**Then** the profile's certificate file is read from disk exactly once and the same parsed
key material is reused for both endpoints.

#### AC6: CA bundle is loaded into a trust pool
**Given** a profile whose `ca` points to a valid CA bundle of two CAs
**When** the provider loads that profile
**Then** a trust pool containing both CAs is available for later peer verification.

#### AC7: No certificate or key material in logs
**Given** debug-level logging and a certificate that loads (and another that fails to load)
**When** the operator inspects the logs
**Then** no certificate body, private key, or passphrase appears in any log line — only
references (profile name, file path) and outcomes.

#### Non-Functional Expectations
- The `TLSProvider` boundary must be narrow enough that an alternative implementation
  (OpenSSL/HSM) can satisfy it with no change to the listener, dialers, or config parsing —
  core logic depends only on the boundary, never on `crypto/tls` directly.
- Certificate loading must not re-read files for cached profiles under repeated use.
