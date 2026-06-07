# [STORY-001-006] Correlation ids & X-Sequencer headers

> Story 006 of the module-001 decomposition of `PRD.md`. See `[User-story-1]` for the
> shared INVEST analysis and split strategy.

### Background
Operators and downstream applications need a stable handle to correlate everything that
belongs to one call across its many legs. The sequencer mints a `call_id` (stable across
the whole chain for one call) and a per-leg `leg_id`, and carries them as informational
headers `X-Sequencer-Call-Id` and `X-Sequencer-Leg-Id` on each outbound leg INVITE. The
SIP `Call-ID`/tags stay the sequencer's internal mapping input; applications may key on
the `X-Sequencer-*` ids for a stable cross-leg handle. This story delivers that
correlation, which underpins logs, metrics, and application-side joins.

Key points:
- Business value: one stable id ties together all legs and logs of a call.
- Applications get a cross-leg handle without parsing internal SIP state.
- Needed now so logs/metrics (later stories) and external apps can correlate.

### Business Value
- Provide a single stable `call_id` spanning every leg of one call for correlation.
- Support applications keying off `X-Sequencer-*` headers for a stable cross-leg handle.
- Enable coherent logging and troubleshooting across a multi-leg call.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-002]` (outbound legs exist to carry headers).
- **Data assumptions:** Outbound INVITEs can carry custom `X-` headers to applications and
  the PBX.
- **Integration points:** External applications and the PBX receive the headers (treated
  as informational; they need not act on them).
- **Business constraints:** Ids are informational; SIP `Call-ID`/tags remain the internal
  mapping (PRD §5). Re-issuing a stable `call_id` across a transfer is out of scope
  (PRD §8).

### Scope In
- Mint one `call_id` per inbound call, stable across the whole chain for that call.
- Mint a distinct `leg_id` per outbound leg.
- Add `X-Sequencer-Call-Id` and `X-Sequencer-Leg-Id` headers to every outbound leg INVITE
  (each application and the PBX).

### Scope Out
- Reissuing `call_id` across a REFER/transfer (new SIP Call-ID) — out of scope (PRD §8).
- Wiring these ids into Prometheus metric labels — `[STORY-001-008]` consumes them.
- Persisting ids anywhere — no storage (PRD §8).

### Acceptance Criteria

#### AC1: Stable call_id across all legs
**Given** a call traversing three applications and the PBX
**When** the outbound INVITE to each hop is observed
**Then** every hop carries the same `X-Sequencer-Call-Id` value for that call.

#### AC2: Distinct leg_id per leg
**Given** the same multi-leg call
**When** the outbound INVITEs are observed
**Then** each leg carries a different `X-Sequencer-Leg-Id` value (unique per leg).

#### AC3: Different calls get different call_ids
**Given** two separate inbound calls
**When** their outbound INVITEs are observed
**Then** the two calls carry different `X-Sequencer-Call-Id` values.

#### AC4: Headers present on every outbound hop including PBX
**Given** a call with one application and the PBX
**When** the INVITE to the application and the INVITE to the PBX are observed
**Then** both carry `X-Sequencer-Call-Id` and `X-Sequencer-Leg-Id` headers.

#### AC5: Internal SIP mapping unaffected
**Given** an established multi-leg call
**When** the legs are correlated internally by the sequencer
**Then** correlation still works using the sequencer's internal SIP `Call-ID`/tags
mapping — the `X-Sequencer-*` headers are informational and do not replace it.

#### Non-Functional Expectations
- `call_id` and `leg_id` values must be unique enough to avoid collisions across the
  target call volume (UUID-grade uniqueness, per PRD §5).
