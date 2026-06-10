# Story Decomposition: SIP TLS transport support (story 5/5)

> Part of the TLS decomposition. Module overview and INVEST analysis in
> `[User-story-12]tls-configuration.md`. Derived from `requirements/support-sip-tls.md`.

---

## [STORY-001-016] Outbound TLS to applications & next hop

### Background
The sequencer originates SIP legs to each external application in the `sequence` chain and
to the terminating next hop (PBX, proxy, carrier). This story lets those outbound
connections run over TLS. An outbound endpoint has exactly one `transport` value, so TLS is
a switch — a `sequence` item or `next_hop` is either TLS or plain, never both at once. When
`transport: tls`, the dialer resolves the named profile, obtains the client context
(`[STORY-001-014]`, including the client certificate for mTLS), validates the remote against
the configured CA, and honours the connect timeout. The same outbound profile referenced by
several endpoints means the same certificate is reused with no duplication. The string-form
`next_hop: host:port` keeps working unchanged; an object form opts a next hop into TLS.

Key points:
- Business value: end-to-end encrypted signaling to applications and the next hop, with
  mutual authentication where required.
- Consumes the client context boundary; completes the TLS feature (server + client).
- Needed now because outbound TLS to applications and the next hop is a primary requirement.

### Business Value
- Provide encrypted, optionally mutually-authenticated outbound signaling to SIP
  applications and to the next hop (proxy/carrier).
- Support per-endpoint choice of TLS or plain transport via a single switch, reusing one
  profile (and thus one certificate) across many endpoints.
- Enable reliable failure behavior — a TLS peer that is unreachable or untrusted fails fast
  under the configured policy rather than hanging the call.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-012]` (resolved outbound profiles), `[STORY-001-014]`
  (client-side TLS context). Existing outbound origination and per-app failure handling
  (`[STORY-001-002]`, `[STORY-001-004]`).
- **Data assumptions:** `sequence` items / `next_hop` with `transport: tls` carry a resolved
  profile; client certificates and CA bundles load via the provider at startup.
- **Integration points:** External SIP application servers and the next-hop destination over
  the network.
- **Business constraints:** Each outbound endpoint is TLS *or* plain (single switch, not
  parallel). An unresolved profile was already rejected at config load (`[STORY-001-012]`),
  not at call time. The string-form `next_hop` stays backward compatible.

### Scope In
- When a `sequence` item or `next_hop` has `transport: tls`, dial it over TLS using the
  resolved profile's client context; when `transport` is plain, dial as today (the switch).
- Present the client certificate for mTLS and validate the remote's certificate against the
  configured CA bundle.
- Honour `connect_timeout` on the connection attempt (`0` = unlimited), failing fast on
  unreachable/slow TLS peers with a clear error.
- Reuse the same certificate when several endpoints reference the same profile (no duplicate
  key material).
- Keep the string-form `next_hop: host:port` working; support the object form
  (`uri` / `transport` / `tls_profile`) for a TLS next hop.

### Scope Out
- Inbound listener — `[STORY-001-015]`.
- Building the client context / verification policy — `[STORY-001-014]`.
- Loading certificates from disk — `[STORY-001-013]`.
- Per-app failure *policy* semantics (skip/abort) themselves — owned by `[STORY-001-004]`;
  this story feeds a TLS failure into that existing policy.
- Certificate reload without restart and certificate pinning (future considerations).

### Acceptance Criteria

#### AC1: Transport switches TLS ↔ plain per endpoint
**Given** a `sequence` item `transcribe` with `transport: tls` `tls_profile: outbound` and,
in a separate configuration, the same item with `transport: tcp`
**When** the sequencer originates the leg to that application
**Then** the `tls` configuration establishes the leg over TLS, and the `tcp` configuration
establishes it over plain TCP — the single `transport` value decides, never both at once.

#### AC2: Mutual TLS presents the configured client certificate
**Given** an outbound profile `outbound` whose `cert`/`key` carry a client certificate and a
remote that requires client authentication
**When** the sequencer dials that remote over TLS
**Then** the sequencer presents the client certificate and the remote accepts the
mutually-authenticated connection.

#### AC3: An untrusted remote certificate is refused and handled per failure policy
**Given** an outbound `sequence` item whose remote presents a certificate not signed by the
profile's configured CA, the item having `on_failure: skip`
**When** the sequencer dials the application
**Then** the TLS connection is refused, the failure is audit-logged (no certificate data),
and the call proceeds under the existing `skip` policy (the application is skipped).

#### AC4: Connect timeout fails fast instead of hanging
**Given** an outbound profile with `connect_timeout: 5s` and a TLS destination that does not
respond
**When** the sequencer attempts to dial it
**Then** the attempt is abandoned after ~5 seconds with a clear timeout error, rather than
blocking the call indefinitely.

#### AC5: One profile reused by several endpoints reuses one certificate
**Given** both a `sequence` item `transcribe` and the `next_hop` referencing the same profile
`outbound`
**When** both outbound legs are established
**Then** both use the same `outbound` certificate, loaded once, with no duplicated key
material.

#### AC6: Backward-compatible string-form next hop still dials plain
**Given** a config with `next_hop: pbx.internal:5060` (string form, no transport)
**When** the sequencer originates the leg to the next hop
**Then** it dials plain exactly as today — no TLS, no profile required.

#### Non-Functional Expectations
- A TLS dialing failure to one endpoint must be confined to that endpoint and resolved
  through the existing per-application failure policy — it must not crash or stall the whole
  call.
- No certificate data appears in logs for outbound handshakes, successful or failed.

---

### Generation summary (TLS decomposition, stories 12–16)
- Feature: SIP TLS transport support (derived from `requirements/support-sip-tls.md`)
- Stories generated: 5 (`[STORY-001-012]` … `[STORY-001-016]`)
- Model: named `tls_profiles` (cert + policy bundle) referenced by name; additive and
  backward compatible.
- See companion files `[User-story-12]` … `[User-story-16]`.
