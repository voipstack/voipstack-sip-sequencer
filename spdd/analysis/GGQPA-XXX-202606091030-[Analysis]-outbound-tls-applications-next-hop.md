# SPDD Analysis: Outbound TLS to applications & next hop

> Source: `requirements/[User-story-16]outbound-tls-applications-next-hop.md` (STORY-001-016),
> derived from `requirements/support-sip-tls.md`. Synced model: per-endpoint `transport`
> switch on `sequence` items and `next_hop`; no DNS SRV/NAPTR, no connection pooling (removed).

## Original Business Requirement

> Reproduced from `requirements/[User-story-16]outbound-tls-applications-next-hop.md`.

When a `sequence` item or `next_hop` has `transport: tls`, the dialer resolves the named
profile, obtains the client context (`[STORY-001-014]`, incl. client cert for mTLS), validates
the remote against the configured CA, and honors `connect_timeout`. One transport per endpoint
(TLS or plain, never both). The same profile reused by several endpoints reuses one
certificate. The string-form `next_hop: host:port` keeps working; an object form opts into TLS.

(Full Business Value / Dependencies / Scope In / Scope Out / Acceptance Criteria AC1–AC6 /
Non-Functional Expectations as in the source story file.)

## Domain Concept Identification

### Existing Concepts (from codebase)
- **`runAppChain`** (`bridge.go:179`): originates each application leg via
  `e.dialogCliCache.Invite(ctx, appURI, body, headers...)`. Crucially, **every app leg is
  forced to TCP** today via `withTCP(appURI)` (`bridge.go:191`) because tap SDP can exceed
  UDP MTU. The TLS switch replaces/augments this per-item transport decision.
- **`dialPBX`** (`bridge.go:423`): originates the next-hop leg via `Invite(ctx, pbxURI, ...)`,
  parsing `e.cfg.NextHop` (scalar string) with `sip.ParseUri`. The next-hop TLS switch lives
  here.
- **`withTCP`** (`bridge.go:19`): sets the URI `transport` param to `tcp`. The existing
  mechanism for selecting transport is **the SIP URI `transport` parameter** — TLS dialing
  will set `transport=tls` similarly (sipgo dials per the URI/transport, using a client
  `*tls.Config` for `tls`).
- **`sipgo.Client`** (`e.cli`, `engine.go:73`): one client; sipgo selects transport per
  request. A client-side `*tls.Config` (from the provider) is needed for `tls` legs.
- **Per-app failure policy** (`failureAction`, `on_failure` skip/abort, `bridge.go:204+`):
  a TLS dial failure feeds this **existing** policy unchanged — owned by `[STORY-001-004]`.
- **`config.Application` + `NextHop`** (post-012): now carry `transport` + optional
  `tls_profile` (and `next_hop` is string-or-object).

### New Concepts Required
- **Outbound transport switch** — per `sequence` item / `next_hop`: `tls` → dial over TLS with
  the resolved client context; otherwise dial as today. A single `transport` field decides.
- **Client `*tls.Config` wiring** — obtain the client context (014) for a TLS leg and hand it
  to the sipgo dial path (client option or per-request).
- **Connect timeout** — apply `connect_timeout` to the TLS connection attempt (`0` =
  unlimited), distinct from the existing `legTimeout` (32s answer timeout, `engine.go:91`)
  which governs SIP answer, not TCP/TLS connect.

### Key Business Rules
- **One transport, switch not parallel** (AC1): the `transport` value alone decides TLS vs plain.
- **mTLS presents client cert** (AC2).
- **Untrusted remote refused → existing failure policy** (AC3): refusal handled by `skip`/`abort`.
- **`connect_timeout` fails fast** (AC4): bounded connect, clear error, no indefinite hang.
- **One profile reused → one certificate** (AC5): cert loaded once (013), reused.
- **String-form `next_hop` still plain** (AC6): backward compatible.

## Strategic Approach

### Solution Direction
- **Thread a client TLS context into the two dial sites** (`runAppChain`, `dialPBX`). For an
  endpoint with `transport: tls`, set the URI `transport=tls` (analogous to `withTCP`) and
  supply the resolved client `*tls.Config` to sipgo for that dial; otherwise keep today's
  path (UDP next hop, forced-TCP app legs).
- **Reconcile with the forced-TCP app rule**: today all app legs are TCP. Under the switch, an
  app with `transport: tls` dials TLS; an app with no/`udp`/`tcp` transport keeps the current
  behavior (note: the MTU rationale that forces TCP still applies to plain tap legs — TLS rides
  TCP, so it also satisfies the MTU concern). Decide whether omitted-transport app legs stay
  forced-TCP (recommended: yes, preserve current behavior) and only switch to TLS on explicit
  `transport: tls`.
- **Connect timeout**: wrap the TLS dial in a context/deadline derived from `connect_timeout`
  (separate from `legTimeout`). On expiry, return a named timeout error into the existing
  failure-policy branch.
- **Cert reuse**: provider cache (013) already guarantees one load per profile/cert; multiple
  endpoints naming `outbound` share it (AC5) with no extra work here.

### Key Design Decisions
- **How sipgo receives the client `*tls.Config`** (pivotal — **resolved against sipgo
  source**): the outbound dial TLS config is **UA-level**, not per-request. `WithUserAgenTLSConfig(*tls.Config)`
  (`ua.go:50`) sets `ua.tlsConfig`, plumbed into `NewTransportLayer(..., ua.tlsConfig, ...)`
  (`ua.go:100`) → `TransportTLS.init(par, dialTLSConf)` (`sip/transport_tls.go:20`). The dial
  path uses that single config and only `Clone()`s it to set SNI `ServerName` from the dial
  host (`transport_tls.go:24-30`). There is **no per-request `*tls.Config`** in the client
  write path (`WithClient*` options carry none). → **Design: one `sipgo.UserAgent` +
  `sipgo.Client` per distinct resolved outbound profile**, keyed by profile name, each built
  with `WithUserAgenTLSConfig(clientCtx)`. The common case (a single `outbound` profile reused
  by many endpoints) is exactly **one** extra UA/client; the engine's existing UA stays the
  plain (udp/tcp) dialer. SNI is handled automatically by the transport per host.
- **Cert reuse (AC5) falls out for free**: one profile → one UA/client → one `*tls.Config` →
  one loaded certificate (provider cache, 013). Shared by every endpoint naming that profile.
- **Connect timeout placement**: distinct from `legTimeout`. → Apply at the transport-connect
  layer (or a `context.WithTimeout` around the TLS dial), not by shrinking the answer timeout.
- **No SRV/NAPTR, no pooling**: removed from scope to match the requirement — dial the
  configured host:port directly; no connection reuse layer. (Were invented; now out.)
- **Failure mapping**: a TLS refusal/timeout maps to the existing originate-failure branch
  (`AppFailure`/`TerminatingHopFailure` metrics + `skip`/`abort`) — no new policy.

### Alternatives Considered
- **One TLS-configured `sipgo.Client` for all legs**: rejected — cannot serve multiple
  profiles (different client certs/CAs) on different endpoints.
- **Reusing `legTimeout` for connect**: rejected — conflates SIP answer time with TCP/TLS
  connect; `connect_timeout` is a distinct, configurable knob.
- **Adding pooling now**: rejected — not in the requirement; YAGNI (AGENTS.md).

## Risk & Gap Analysis

### Requirement Ambiguities
- **R1 — sipgo per-dial TLS config mechanism (RESOLVED).** sipgo v1.4.0 has no per-request
  TLS config; the dial config is UA-level (`WithUserAgenTLSConfig`). Design is therefore
  **one UA+Client per distinct outbound profile** (see Key Design Decisions). No longer an
  open question.
- **R2 — omitted-transport app legs.** Do app items with no `transport` remain forced-TCP
  (current behavior) or become plain UDP? Recommend **preserve forced-TCP** (don't regress the
  MTU fix); only `transport: tls` changes the leg to TLS.
- **R3 — `connect_timeout` scope.** Does it bound only the TCP/TLS connect, or connect +
  handshake? Recommend connect + handshake (the operator's intent is "don't hang on a dead
  TLS peer").

### Edge Cases
- App with `transport: tls` **and** `media: tap`: TLS rides TCP, so the MTU concern is met;
  ensure the tap SDP path is unaffected by the transport switch.
- `next_hop` object with `transport: tls` but profile cert is client-auth-less and remote
  demands mTLS: surfaces as a handshake failure → failure policy (next hop abort).
- `connect_timeout: 0` (unlimited) to an unreachable TLS host: relies on OS/sipgo default
  connect timeout — document that `0` means "no extra deadline", not "instant".
- Remote presents valid cert from an unconfigured CA (`ca` omitted, system roots used):
  ties to story-014 R "no CA → system roots vs fail closed".

### Technical Risks
- **UA/Client-per-profile lifecycle** (was R1, now resolved): building and owning one extra
  `sipgo.UserAgent`+`Client` per outbound profile — construction, `Close()` on shutdown
  (extend `engine.go:164`), and a small profile→client map. Low-medium; the engine already
  owns a UA/client pair, this generalizes it. Keep ownership explicit (`go test -race`).
- **Routing a leg to the right client**: `runAppChain`/`dialPBX` must select the plain client
  or the profile's TLS client per endpoint. Mechanical but touches the hot dial path.
- **Touching the hot dial path** (`runAppChain`/`dialPBX`): central call-setup code; changes
  must preserve the existing plain/TCP behavior exactly and keep failure-policy semantics
  (existing tests must stay green). Reproduce-via-test per AGENTS.md before changing.
- **Testing outbound TLS**: a TLS-listening fake UAS (real cert testdata), mirroring
  `newFakeUASTCP`. External peers are the mock boundary; failure policy tested for real.
- **No data-integrity/concurrency concern** beyond per-leg context/deadline lifecycle.

### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Switch TLS ↔ plain per endpoint | Yes | Route leg to plain client or the profile's TLS client (UA-per-profile). |
| AC2 | mTLS presents client cert | Yes | Client cert in the per-profile UA's `*tls.Config` (014). |
| AC3 | Untrusted remote refused → failure policy | Yes | Maps to existing originate-failure branch. |
| AC4 | `connect_timeout` fails fast | Yes | `context.WithTimeout` around the TLS dial; R3 (scope). |
| AC5 | One profile reused → one certificate | Yes | Provider cache (013) guarantees single load. |
| AC6 | String-form `next_hop` dials plain | Yes | Backward-compat path unchanged (`dialPBX`). |

**Summary:** All 6 ACs addressable. **R1 resolved** against sipgo source: outbound TLS config
is UA-level, so the design is **one UA+Client per distinct outbound profile** (common case =
one extra client). Remaining small decisions before REASONS Canvas: R2 (omitted-transport app
legs stay forced-TCP) and R3 (`connect_timeout` bounds connect + handshake).
