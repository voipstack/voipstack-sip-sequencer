# Support SIP TLS

## Overview

Add TLS transport to the SIP application sequencer for both inbound and outbound
connections (sequence applications and the terminating next hop). TLS is configured
via YAML using **named `tls_profiles`** — reusable bundles of TLS policy (certificate
+ crypto + verification + timeouts). Anything that does TLS — the inbound listener,
each sequence item, the next hop — names a profile via `tls_profile:`. One profile
is reused by many endpoints, so the same certificate serves multiple outbound
connections with zero duplication.

## Functional Requirements

### Backward compatibility (TLS is opt-in)

- **All TLS configuration is optional and additive.** A config with no `tls:`,
  `tls_profiles:`, `transport:`, or `tls_profile:` keys parses and behaves exactly
  as today (plain SIP only).
- Every new field is optional; its zero value means "no TLS" / "plain transport".
- Omitting `tls.listen` ⇒ no TLS listener. Omitting an item's `transport` ⇒ plain (`udp`).
- Existing string-form `next_hop: host:port` keeps working unchanged.

### Inbound (server-side)

- Plain SIP (`sip.listen`) and TLS (`tls.listen`) listen on **separate ports in parallel**.
- The TLS listener names a `tls_profile` supplying its server certificate and policy.
- Accept TLS connections, negotiate the handshake, verify peer certs when configured (mTLS).

### Outbound to sequence applications

- Each `sequence` item may use TLS by setting `transport: tls` + `tls_profile:`.
- An item has exactly **one transport** — TLS or plain, never both (switch, not parallel).
- The profile supplies the client certificate and validation policy for that hop.

### Outbound to next hop

- The terminating next hop may use TLS via `transport: tls` + `tls_profile:`.
- One transport only — TLS or plain (switch, not parallel).

## Configuration (YAML)

```yaml
# config.yaml — complete instance configuration
sip:
  listen: 0.0.0.0:5060          # plain SIP listen address/port

tls:
  listen: 0.0.0.0:5061          # TLS listen — runs in PARALLEL with sip.listen
  tls_profile: inbound          # server cert + policy for this listener

next_hop:                       # terminating next-hop (PBX) — switchable, not parallel
  uri: sip:pbx.internal:5060
  transport: tls                # tls | udp | tcp (default udp)
  tls_profile: outbound

rtp:
  port_range: 10000-20000       # anchored media port range

observability:
  listen: 0.0.0.0:9090          # Prometheus /metrics + /health (omit to disable)

log_level: info                 # debug | info | warn | error (default info)

# Named, reusable TLS policy. One profile → many endpoints → same certificate reused.
tls_profiles:
  inbound:                      # server-side (presents server cert; optional mTLS)
    cert: /etc/sip/server.crt
    key:  /etc/sip/server.key
    # policy omitted → secure defaults (TLS 1.2 min, no peer verify)

  outbound:                     # client-side (presents client cert, validates remote)
    cert: /etc/sip/client.crt
    key:  /etc/sip/client.key
    passphrase: ""              # optional, for encrypted key files
    ca:   /etc/sip/ca-bundle.crt
    min_version: tlsv1.2        # contiguous floor; max is tlsv1.3
    ciphers:                    # applied to TLS 1.2 only (1.3 ciphers are fixed)
      - TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
      - TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
      - TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384
    verify_depth: 2
    verify_dates: true
    connect_timeout: 5s

sequence:                       # ordered application chain — list order IS chain order
  - name: transcribe
    uri: sip:transcriber.internal:5060
    on_failure: skip            # skip | abort
    media: tap                  # tap | none (default none)
    transport: tls              # tls | udp | tcp (default udp)
    tls_profile: outbound       # client cert + policy for this hop
  - name: record
    uri: sip:recorder.internal:5060
    on_failure: skip
    media: tap
  - name: route-guard
    uri: sip:guard.internal:5060
    on_failure: abort
    media: none
```

**One rule everywhere:** anything doing TLS names a `tls_profile` — the `tls.listen`
listener and any `sequence` item / `next_hop` with `transport: tls`.

**Validation (parse time):**
- `transport: tls` (or a `tls.listen` block) ⇒ a `tls_profile` is required.
- A referenced `tls_profile` must exist in the `tls_profiles` block.
- A profile's `cert`/`key` files must be loadable (incl. `passphrase` if encrypted).
- Any unresolved reference fails the config load with a clear error.

## Non-Functional Requirements

### Scalability / Architecture

- **Abstraction layer** so the underlying TLS library can be swapped without touching core logic.
- A single TLS boundary owns: certificate loading (including encrypted keys), building the server-side and client-side TLS settings, and certificate validation (peer, chain depth, dates, subjects).
- The default implementation uses the language's standard TLS library.
- A clear boundary lets an alternative library (e.g. an OpenSSL-backed or hardware-security-module variant) be substituted with no change to listeners, dialers, or config parsing.

### Performance

- Load + parse each profile's certificate once; reuse across all endpoints naming it.
- Plain connections unaffected by TLS overhead.

### Security

- TLS 1.2 minimum by default; reject weaker versions and weak cipher suites.
- Validate peer certificates when configured; log validation failures with context.
- Never emit certificate/key material to logs.

## Design Notes

### Model

- **tls_profiles** — named bundles: certificate (`cert`/`key`/optional `passphrase`/optional `ca`) plus crypto/verification/timeout policy. Omitted fields use the standard library's secure defaults.
- **endpoints** — the `tls.listen` listener, each `sequence` item, and `next_hop` reference a profile by name when doing TLS.
- Reuse: one profile is shared by many endpoints → the same certificate serves them all. (A separate certificate registry is not needed until two profiles must share one cert file with different policy — add it then, not now.)

### Inbound Listeners

- `sip.listen` (plain) and `tls.listen` (TLS) are independent sockets on different ports — parallel by construction.
- The `tls.listen` block names a profile supplying the server certificate.
- On accept, the connection is wrapped TLS.

### Outbound Connections

- Each `sequence` item and `next_hop` has exactly one `transport` → TLS or plain, never both (switch, not parallel — falls out of a single field).
- When `transport: tls`, the dialer resolves the named profile → certificate → establishes the TLS connection.
- A missing/unresolved profile fails fast at config load, not at call time.

### TLS Profile Fields

All fields below live on a `tls_profile`. Endpoints inherit them by name.

**Encryption**

- **min_version** — minimum TLS version. Default `tlsv1.2`; maximum is `tlsv1.3`. Only a contiguous minimum-to-maximum range is supported — you cannot enable an arbitrary set (e.g. allow 1.0 but skip 1.1). Lower versions are rejected at handshake.
- **ciphers** — allowlist of cipher-suite names, **applied to TLS 1.2 only**. Omit for secure defaults. TLS 1.3 cipher suites are fixed (AES-128-GCM, AES-256-GCM, ChaCha20-Poly1305) and cannot be restricted — this field is ignored on 1.3 handshakes.
- **passphrase** — optional, decrypts password-protected key files. Encrypted keys require a crypto dependency beyond the base standard library.

**Validation**

- **verify_peer** — require and verify the peer certificate (mTLS). Default `false`. Meaningful on the inbound listener.
- **verify_depth** — maximum certificate chain depth. Default `2`. Enforced during certificate verification (no built-in toggle; handled in validation logic).
- **verify_dates** — check Not Before / Not After. Default `true` (always checked unless explicitly disabled). Disabling requires a custom verification path.
- **verify_subjects** — allowlist of certificate subjects (mTLS pinning). Default empty (any subject). Checked against the peer certificate's subject during verification.

**Connection**

- **connect_timeout** — outbound connection attempt timeout. Duration string (e.g. `5s`). Default `0` (unlimited).

### TLS Provider Abstraction

A single boundary isolates the TLS library so it can be swapped without touching core
logic. The provider's job:

- **Load a certificate** from a profile (files + optional passphrase + CA).
- **Build a server-side TLS context** from a resolved inbound profile (cert + min version + ciphers + peer verification + subjects).
- **Build a client-side TLS context** from a resolved outbound profile (cert + min version + ciphers + chain/date verification + connect timeout).

The config layer resolves `tls_profile` references into a flat, library-agnostic
value. The TLS boundary consumes it and returns whatever the underlying library needs.
The default implementation uses the standard TLS library; an alternative implements
the same boundary with no change to the rest of the system.

## Notes for Implementation

All additions are optional; a config with no TLS keys behaves exactly as today.

- **Existing keys are unchanged** — `sip.listen`, `next_hop`, `rtp.port_range`, `sequence` (`name`/`uri`/`on_failure`/`media`), `log_level`, `observability.listen` keep their current meaning and defaults.
- **New keys are additive** — `tls` (listener), `tls_profiles` (named policy), and per-endpoint `transport` / `tls_profile`. Absent ⇒ plain transport, no TLS.
- **`next_hop` stays backward compatible** — the plain `host:port` string form keeps working; an object form adds `transport` / `tls_profile` only when TLS is wanted.
- **Defaults** — an endpoint with no `transport` is plain. A profile's omitted policy uses secure defaults (TLS 1.2 minimum, no peer verification).
- **Validation at load** — `transport: tls` (and the `tls` listener) require a `tls_profile`; the named profile must exist. Failures are reported at config load with a clear message, consistent with the existing "missing required key" style.
