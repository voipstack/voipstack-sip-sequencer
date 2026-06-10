# Story Decomposition: SIP TLS transport support

> These files (`[User-story-12]` … `[User-story-16]`) extend the **module-001**
> decomposition of the sequencer with TLS transport, derived from
> `requirements/support-sip-tls.md`. Same pattern as stories 10/11: additional
> capabilities of the existing B2BUA, numbered `STORY-001-012` … `STORY-001-016`.

## INVEST Analysis

### Abstract Task
**Feature Name:** SIP TLS transport (inbound TLS listener + outbound applications & next hop)

**Analysis Dimensions**
- **Core Responsibility:** Carry SIP signaling over TLS, both as a server (a TLS listener
  running in parallel with the existing plain listener) and as a client (outbound to SIP
  applications in the sequence and to the next hop). TLS policy is expressed in YAML
  through **named `tls_profiles`** — reusable bundles of certificate + crypto +
  verification + timeout policy — that endpoints reference by name. All TLS config is
  optional and additive: a config with no TLS keys behaves exactly as today.
- **Primary Operations:** parse & validate the `tls_profiles` block and per-endpoint
  `transport`/`tls_profile` fields; load each profile's certificate (incl. encrypted
  keys) once and reuse it; build server/client TLS contexts with the configured crypto &
  verification policy; open the TLS listener; dial outbound TLS.
- **Key Constraints:** inbound TLS and plain listen on separate ports in parallel;
  outbound transport is a single switch per endpoint (TLS or plain, never both);
  contiguous TLS version range only (min `tlsv1.2`, max `tlsv1.3`); ciphers configurable
  for TLS 1.2 only; a TLS library boundary (`TLSProvider`) so the implementation can be
  swapped; no certificate data in logs; full backward compatibility (string-form
  `next_hop`, unchanged `sip.listen` / `sequence`).
- **Technical Complexity:** High (TLS handshakes, mTLS, chain/subject/date verification
  via callbacks, dialing).
- **Business Complexity:** Medium (named-profile reuse model, parse-time validation,
  backward compatibility).

### INVEST Evaluation (whole feature)
- ❌ **Independent** — spans config, a TLS abstraction, the server listener, and client dialers.
- ✅ **Negotiable**
- ✅ **Valuable** — encrypted SIP signaling end to end.
- ❌ **Small** — multi-week.
- ✅ **Testable**

**Conclusion:** Needs splitting.

### Split Strategy
Split **by capability** (not by technical layer). 5 stories, layered so each delivers
independent, testable value and the next builds on a stable boundary:
1. `[STORY-001-012]` TLS configuration — `tls_profiles` model, reference resolution, parse-time validation, backward compatibility (this file)
2. `[STORY-001-013]` TLS provider boundary & certificate loading (load cert/key, encrypted keys, CA; load once & reuse)
3. `[STORY-001-014]` TLS context construction & certificate verification policy (server & client)
4. `[STORY-001-015]` Inbound TLS listener (parallel with plain, mTLS enforcement)
5. `[STORY-001-016]` Outbound TLS to applications & next hop (switch per endpoint, connect timeout)

---

## [STORY-001-012] TLS configuration — `tls_profiles` model & validation

### Background
Operators configure the sequencer through one central YAML file (`[STORY-001-001]`).
TLS adds **named `tls_profiles`** to that file: each profile is a reusable bundle of a
certificate (`cert` / `key` / optional `passphrase` / optional `ca`) plus crypto,
verification, and timeout policy. Anything that does TLS — the `tls.listen` listener, a
`sequence` item, or `next_hop` — names a profile via `tls_profile:`. One profile is reused
by many endpoints, so the same certificate serves many connections with zero duplication.

All TLS configuration is optional and additive: a config with no `tls:`, `tls_profiles:`,
`transport:`, or `tls_profile:` keys parses and behaves exactly as today (plain SIP only).
The existing scalar `sip.listen`, the `sequence` list, and the string-form
`next_hop: host:port` keep their current meaning; the new `tls` listener block, the
`tls_profiles` block, and per-endpoint `transport` / `tls_profile` are added alongside them.

This story delivers the parser and validator only: it turns the YAML into resolved,
library-agnostic profile values (profile ← endpoint) with all defaults applied, and fails
the config load with a clear error on any unresolved reference. Building TLS contexts,
opening sockets, and performing handshakes is downstream work; loading the certificate
files themselves is `[STORY-001-013]`, invoked at startup so an unloadable cert still
fails fast before traffic is served.

Key points:
- Business value: TLS policy is declared once and reused, version-controlled like the rest
  of the config; misconfiguration fails at startup, not mid-call.
- Foundation for every other TLS story — they all consume the resolved profile value.
- Fully backward compatible — existing plain-SIP configs are unaffected.

### Business Value
- Provide operators a single, declarative way to express TLS policy with no duplication
  (one profile → many endpoints → same certificate).
- Preserve every existing config unchanged — TLS is strictly opt-in and additive.
- Enable fast, unambiguous failure when a TLS reference is broken (fail at startup, naming
  the offending reference, not at call time).

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-001]` Configuration loading (this extends the same loader).
- **Data assumptions:** Operator supplies the optional `tls` listener block, the
  `tls_profiles` block, and per-endpoint `transport` / `tls_profile` fields in the existing
  YAML file.
- **Integration points:** None external — config parsing only. (Certificate file loading is
  `[STORY-001-013]`.)
- **Business constraints:** Only the central config file influences behavior (no env vars).
  TLS version range is contiguous (min `tlsv1.2`, max `tlsv1.3`).

### Scope In
- Parse the optional `tls_profiles` block: each name → `cert`, `key`, optional `passphrase`,
  optional `ca`, plus policy (`min_version`, `ciphers`, `verify_peer`, `verify_depth`,
  `verify_dates`, `verify_subjects`, `connect_timeout`).
- Parse the optional `tls` listener block (`listen` + `tls_profile`) alongside the existing
  plain `sip.listen`.
- Parse the per-endpoint `transport` field on `sequence` items and on `next_hop`, with an
  optional `tls_profile` reference; keep the string-form `next_hop: host:port` working and
  add an object form (`uri` / `transport` / `tls_profile`).
- Resolve each TLS endpoint to a single flat, library-agnostic profile value by joining
  endpoint → profile; the same profile referenced by several endpoints resolves to one
  shared certificate identity (no duplication).
- Apply defaults for omitted policy fields: `min_version` = `tlsv1.2`, `ciphers` = Go secure
  defaults (omitted), `verify_peer` = false, `verify_depth` = 2, `verify_dates` = true,
  `verify_subjects` = empty, `connect_timeout` = `0` (unlimited).
- Default transport to plain (`udp`) when `transport` is omitted.
- Enforce the parse-time validation rules and fail the load with a clear, specific error:
  `transport: tls` (and a `tls.listen` block) require a `tls_profile`; a referenced
  `tls_profile` must exist in `tls_profiles`.

### Scope Out
- Reading certificate/key/CA files from disk, parsing PEM, decrypting keys — `[STORY-001-013]`.
- Building `tls.Config` / TLS contexts — `[STORY-001-014]`.
- Opening the listener or dialing — `[STORY-001-015]`, `[STORY-001-016]`.
- Certificate reload without restart (future consideration).
- Validating certificate *content* (expiry, chain) — that is handshake-time behavior.

### Acceptance Criteria

#### AC1: A config with no TLS keys behaves exactly as today
**Given** an existing config with `sip.listen`, a `sequence`, a string-form
`next_hop: host:port`, and no `tls`, `tls_profiles`, `transport`, or `tls_profile` keys
**When** the process starts
**Then** startup succeeds and behaves exactly as before — plain SIP only, no TLS listener,
every endpoint plain transport.

#### AC2: Resolve a profile reused by several endpoints to one shared certificate
**Given** a `tls_profiles` block with a profile `outbound`, a `sequence` item `transcribe`
with `transport: tls` `tls_profile: outbound`, and a `next_hop` object with `transport: tls`
`tls_profile: outbound`
**When** the process starts
**Then** startup succeeds; both the application and the next hop carry a fully resolved TLS
profile; and both resolve to the same `outbound` certificate (no duplication).

#### AC3: `transport: tls` without a profile name fails fast
**Given** a `sequence` item with `transport: tls` and no `tls_profile`
**When** the process starts
**Then** startup fails immediately with an error stating a `tls_profile` is required for the
TLS endpoint, naming the item, and no connection is attempted.

#### AC4: `tls.listen` without a profile name fails fast
**Given** a `tls` block with `listen: 0.0.0.0:5061` and no `tls_profile`
**When** the process starts
**Then** startup fails immediately with an error stating a `tls_profile` is required for the
TLS listener, and no listener is opened.

#### AC5: Reference to a non-existent profile fails fast
**Given** a `sequence` item with `transport: tls` `tls_profile: missing`, where `missing` is
not defined in `tls_profiles`
**When** the process starts
**Then** startup fails immediately with an error naming the unresolved profile `missing` and
the item that referenced it.

#### AC6: TLS and plain listeners coexist (parallel by construction)
**Given** `sip.listen: 0.0.0.0:5060` and a `tls` block `listen: 0.0.0.0:5061`
`tls_profile: inbound`
**When** the process starts
**Then** startup succeeds and both listener configurations are present — a plain listener on
5060 and a TLS listener on 5061, neither replacing the other.

#### AC7: Omitted policy fields take secure defaults
**Given** a profile `inbound` that names only `cert` and `key` and omits all policy fields
**When** the process starts
**Then** the resolved profile has `min_version` = `tlsv1.2`, peer verification disabled,
`verify_depth` = 2, `verify_dates` enabled, no subject restriction, and `connect_timeout` =
`0`.

#### AC8: Backward-compatible `next_hop` — string and object forms
**Given** one config with `next_hop: pbx.internal:5060` (string) and a second config with a
`next_hop` object `uri: sip:pbx.internal:5060` `transport: tls` `tls_profile: outbound`
**When** each process starts
**Then** both succeed: the string form resolves to a plain next hop unchanged, and the
object form resolves to a TLS next hop carrying the `outbound` profile.

#### AC9: Non-TLS endpoints need no profile
**Given** a `sequence` item with `transport: udp` (or no `transport`) and no `tls_profile`
**When** the process starts
**Then** startup succeeds and the item is configured plain; the absence of a `tls_profile`
is not an error for non-TLS transports.

#### Non-Functional Expectations
- Every TLS configuration failure message must name the offending reference (endpoint or
  profile) so an operator can fix the file without reading source code.
- Reference wiring is validated here; the certificate files a profile names are loaded by
  the provider (`[STORY-001-013]`) at startup, so an unloadable certificate still fails fast
  before any traffic is served.
