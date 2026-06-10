# SPDD Analysis: TLS configuration — `tls_profiles` model & validation

> Source: `requirements/[User-story-12]tls-configuration.md` (STORY-001-012), derived from
> `requirements/support-sip-tls.md`. Regenerated to match the synced **two-layer
> `tls_profiles`** model (additive, backward compatible). Supersedes the prior three-layer
> analysis; risk R1 (endpoint re-model) is resolved — the requirement is explicitly additive.

## Original Business Requirement

> Reproduced verbatim from `requirements/[User-story-12]tls-configuration.md`.

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

(Full Business Value / Dependencies / Scope In / Scope Out / Acceptance Criteria AC1–AC9 /
Non-Functional Expectations as in the source story file.)

## Domain Concept Identification

### Existing Concepts (from codebase)
- **Config** (`internal/config/config.go`): the validated in-memory YAML root all stories
  consume. Owns `SIP`, `NextHop` (scalar string URI), `RTP`, `Sequence`, `LogLevel`,
  `Observability`. The TLS work extends this struct additively.
- **SIP.Listen** (scalar `"0.0.0.0:5060"`): the single plain listen address. The synced
  model keeps it as-is and adds a sibling scalar `tls.listen` — no list re-model.
- **NextHop** (scalar SIP URI string, consumed at `bridge.go` `dialPBX`): the synced model
  keeps the string form working and adds an optional object form (`uri` / `transport` /
  `tls_profile`). This is the only place `next_hop` shape changes.
- **Application / Sequence**: ordered chain; each entry has `name`, `uri`, `on_failure`,
  `media`. The synced model adds optional `transport` + `tls_profile` per item — no map
  re-model; the existing list and keys are untouched.
- **String-typed enum + `validate` switch** (`FailurePolicy`, `MediaMode`, `LogLevel`):
  the established closed-set pattern (named consts + exhaustive `switch` in `validate`).
  `transport` and `min_version` are the same shape.
- **Loader pipeline** (`Parse(data, source)` → `Load(path)`): strict decode with
  `dec.KnownFields(true)`; a `rawConfig` mirror with **pointer fields** to distinguish "key
  absent" from "zero value"; `applyDefaults` then `validate`; errors built with
  `fmt.Errorf("... %q ...")` naming the offending key/index. The TLS work extends exactly
  this pipeline.
- **`net.SplitHostPort` validation helper**: already validates `observability.listen`; the
  same fits `tls.listen`.

### New Concepts Required
- **TLS profile** — named bundle: certificate fields (`cert`, `key`, optional `passphrase`,
  optional `ca`) **plus** policy (`min_version`, `ciphers`, `verify_peer`, `verify_depth`,
  `verify_dates`, `verify_subjects`, `connect_timeout`). Reused by many endpoints. This is
  the single reuse joint (one profile → many endpoints → same certificate). No separate
  `certificates` layer — deferred per the requirement's Model note.
- **TLS listener block** (`tls.listen` + `tls_profile`) — optional sibling of `sip.listen`.
- **Transport** — per-endpoint closed enum (`udp` | `tcp` | `tls`); `tls` triggers a
  required profile reference. Default `udp` when omitted.
- **Endpoint TLS reference** — the optional `tls_profile` name on `tls.listen`, a `sequence`
  item, or `next_hop`.
- **Resolved TLS profile** — the loader's output for a TLS endpoint: a flat,
  library-agnostic value joining endpoint → profile with all defaults applied. Contract
  consumed by stories 013–016; must not leak `crypto/tls` types.
- **TLS version** — closed enum, contiguous `min`..`max` (min default `tlsv1.2`, max fixed
  `tlsv1.3`).

### Key Business Rules
- **No TLS keys ⇒ behaves as today** (AC1) — the additive/opt-in invariant.
- **`transport: tls` (and `tls.listen`) ⇒ `tls_profile` required** (AC3, AC4).
- **Referenced profile must exist** (AC5).
- **One profile → many endpoints → same certificate** (AC2): resolution dedupes to the same
  certificate identity, not duplicate descriptors.
- **Secure defaults on omission** (AC7).
- **`next_hop` string ⇔ object both valid** (AC8): string = plain, unchanged; object = opt-in TLS.
- **Default transport `udp`; non-TLS endpoints carry no profile** (AC9).
- **No certificate-content I/O at parse time**: parse validates reference wiring; the named
  cert files are loaded by the provider at startup (`[STORY-001-013]`), still fail-fast.

## Strategic Approach

### Solution Direction
- **Extend `internal/config` in place**, reusing its pipeline: strict `KnownFields(true)`
  decode → `rawConfig` pointer-presence mirror → `applyDefaults` → `validate` with
  reference-naming errors. New optional top-level blocks `tls` (listener) and `tls_profiles`
  (named bundles); a new `Transport` field + optional `TLSProfile` ref on `Application`; a
  `next_hop` that decodes as either a string or an object (custom `UnmarshalYAML`).
- **Resolution step**: after decode, join each TLS endpoint → its profile, apply policy
  defaults, and produce a flat **Resolved TLS profile** attached per endpoint. Cross-ref
  validation (profile-required, profile-exists) runs here so failures are fail-fast and name
  the offending reference.
- **Data flow**: YAML bytes → decode raw → resolve & validate references → `Config` carrying
  per-TLS-endpoint resolved profiles → consumed by 013 (load) / 014 (context). No
  `crypto/tls` import enters `internal/config`.

### Key Design Decisions
- **Additive vs. re-model (resolved):** keep scalar `sip.listen`, scalar/string `next_hop`,
  and the `sequence` list; add `tls.listen`, `tls_profiles`, and per-endpoint
  `transport`/`tls_profile`. → **Additive.** The synced requirement mandates backward
  compatibility (AC1, AC8); no breaking change, no ripple into the engine's listener/dialer
  shapes. (This retires prior risk R1.)
- **`next_hop` dual form (string | object):** implement `UnmarshalYAML` on a `NextHop` type
  that accepts a scalar (→ plain) or a mapping (→ uri/transport/tls_profile). → **Recommended**;
  it is the standard Go-YAML idiom for backward-compatible scalar-or-struct and isolates the
  compatibility logic to one decoder.
- **Where the resolved profile lives:** per-endpoint resolved value vs. a name-keyed lookup
  map. → **Per-endpoint resolved value** — matches the "flat, library-agnostic profile"
  contract; downstream listener/dialer consume one value without re-joining. Shared-cert
  identity (AC2) preserved by resolving to the same certificate descriptor.
- **`connect_timeout` parsing:** parse to a real duration at load (fail fast on a bad
  duration) vs. carry the raw string. → **Parse at load**, consistent with fail-at-startup.
  Note: only `connect_timeout` exists — there is no idle/inactivity timeout in the requirement.

### Alternatives Considered
- **Flat TLS fields on each endpoint** (no `tls_profiles`): rejected — destroys the reuse
  model and reintroduces duplication.
- **Separate `certificates` layer now**: rejected — the requirement explicitly defers a
  certificate registry until two profiles must share one cert file with different policy.
- **Lazy reference resolution at first connection**: rejected — violates AC3–AC5 / fail-at-startup.
- **A separate TLS config file**: rejected — single central config file is the sole source.

## Risk & Gap Analysis

### Requirement Ambiguities
- **R1 — `passphrase: ""`.** Example shows explicit empty string. Treat empty/omitted as
  "no passphrase"; document it.
- **R2 — cipher-name validation at parse time.** Validation rules only mention reference
  existence. Decide now whether unknown cipher constant names are rejected at load (recommend:
  validate against the known Go 1.2 cipher constant set, naming the offending value) or
  carried to `[STORY-001-014]`.
- **R3 — `verify_subjects` syntax** (`CN=phone.internal`): parsed/validated at load, or
  carried opaque to `[STORY-001-014]`? Story scope is parse-only; recommend carrying opaque
  but rejecting empty entries.
- **R4 — `tls_profile` on a `udp`/`tcp` endpoint** (contradictory): reject or ignore? AC9
  covers only the absence case. Recommend rejecting with a naming error.

### Edge Cases
- `min_version` outside the contiguous range (`tlsv1.0`, or `tlsv1.3` as a floor): needs a
  deterministic "unsupported version" error.
- Duplicate keys in `tls_profiles`: YAML v3 errors on duplicate map keys — verify, don't assume.
- A `tls_profile` defined but referenced by no endpoint: should load cleanly (not an error).
- `transport` omitted: default `udp` so existing configs keep working (AC1).
- `next_hop` object missing `transport` but with `tls_profile`, or `transport: tls` without
  `tls_profile`: must hit the same profile-required validation as `sequence` items.

### Technical Risks
- **`KnownFields(true)` strictness**: every new key (`tls`, `tls_profiles`, `transport`,
  `tls_profile`, all profile fields) must be represented in the structs or decoding rejects
  valid TLS configs. Hard requirement, low complexity.
- **`next_hop` becomes a typed `UnmarshalYAML`**: touches the `Config.NextHop` field type
  (currently a plain `string` consumed at `bridge.go` `dialPBX` via `sip.ParseUri`). Must
  stay assignable/compatible so the engine's existing plain path is unchanged.
- **No concurrency/data-integrity concern**: parsing is single-shot at startup, pure. Aligns
  with AGENTS.md (pure core, side effects at edges).
- **Cross-story contract**: the resolved-profile shape is consumed by 013–016; keep it
  minimal and library-agnostic to avoid churn.

### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | No TLS keys ⇒ as today | Yes | Pure additive; pointer-presence mirror already distinguishes absent keys. |
| AC2 | Profile reused → one shared cert | Yes | Resolution dedupes to same cert descriptor. |
| AC3 | `transport: tls` w/o profile fails | Yes | New `switch`/check in `validate`, naming the item. |
| AC4 | `tls.listen` w/o profile fails | Yes | Same, for the listener block. |
| AC5 | Non-existent profile fails | Yes | Cross-ref check, existing naming-error style. |
| AC6 | TLS + plain listeners coexist | Yes | Two scalar blocks; both carried into Config. |
| AC7 | Omitted policy → secure defaults | Yes | Fits `applyDefaults`; confirm `connect_timeout`=0 representation. |
| AC8 | `next_hop` string + object | Yes | Custom `UnmarshalYAML`; verify engine plain path unchanged. |
| AC9 | Non-TLS endpoints need no profile | Yes | Default `udp`; absence not an error. |

**Summary:** All 9 ACs are directly addressable with the existing loader patterns under the
additive model — no endpoint re-model required. Resolve R1–R4 (small decisions) before
REASONS Canvas. The only non-trivial mechanics are the `next_hop` scalar-or-object decoder
and threading resolved profiles per endpoint.
