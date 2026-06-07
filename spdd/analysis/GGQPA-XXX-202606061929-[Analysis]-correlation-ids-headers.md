# SPDD Analysis: Correlation ids & X-Sequencer headers

> Phase 0 (analysis) for `[STORY-001-006]`. Strategic level. Small, localized story.

## Codebase grounding (working notes)
- **Stories 001–005, 010, 011 implemented.** Relevant existing code in `internal/b2bua`:
  - `engine.go`: `newCallID()` mints an 8-byte hex string; used as `Call.id` (registry key +
    teardown id). This **is** the conceptual call_id — just not yet emitted on the wire.
  - `bridge.go`: two outbound-INVITE call sites — app leg (`e.dialogCliCache.Invite(ctx,
    appURI, inviteBody)`) and PBX leg (`...Invite(ctx, pbxURI, pbxOffer)`). Neither passes
    headers today.
  - `proxy.go` (story 011): stateless forward of unmanaged methods — **not calls**; out of
    scope for correlation headers.
- **sipgo API (verified):** `DialogClientCache.Invite(ctx, recipient sip.Uri, body []byte,
  headers ...sip.Header)` — **accepts variadic `sip.Header`**. So attaching custom headers
  is a one-liner per call site (`sip.NewHeader("X-Sequencer-Call-Id", ...)`). No new dep
  needed for header emission; `github.com/google/uuid` is already an (indirect) module dep.
- **No `X-Sequencer-*` headers or per-leg id exist yet.**
- `AGENTS.md`: pure id minting is trivially pure; header attach is at the bridge edge; real
  fakes assert received headers; `-race`.

## Original Business Requirement

> Complete `[STORY-001-006]` text — see `requirements/[User-story-6]correlation-ids-headers.md`.
> Summary: mint one `call_id` (stable across the whole chain for one call) and a per-leg
> `leg_id`; carry them as **informational** headers `X-Sequencer-Call-Id` /
> `X-Sequencer-Leg-Id` on **every outbound leg INVITE** (each app + the PBX). SIP
> `Call-ID`/tags stay the internal mapping. ACs: stable call_id across all legs (AC1);
> distinct leg_id per leg (AC2); different calls ⇒ different call_id (AC3); headers on every
> outbound hop incl. PBX (AC4); internal SIP mapping unaffected (AC5). NFR: UUID-grade
> uniqueness.

## Domain Concept Identification

#### Existing Concepts (from codebase)
- **`Call.id` / `newCallID()`**: already the per-call stable id (registry key). This story
  **exposes** it as `X-Sequencer-Call-Id` (and optionally upgrades it to a UUID per PRD §5).
- **`bridge` outbound INVITE sites** (app leg, PBX leg): the two places headers attach.
- **SIP `Call-ID`/tags**: sipgo-managed internal dialog identity — **untouched**; remains
  the correlation mapping input (AC5).

#### New Concepts Required
- **leg_id:** a fresh id minted **per outbound leg** (each app leg + the PBX leg), emitted
  as `X-Sequencer-Leg-Id`. Conceptually like `newCallID` but per leg.
- **Correlation headers:** the two informational `X-Sequencer-*` headers attached to every
  outbound INVITE.

#### Key Business Rules
- **Stable call_id across the chain:** the same `X-Sequencer-Call-Id` on every outbound leg
  of one call. AC1/AC4. (= `Call.id`.)
- **Distinct leg_id per leg:** a unique `X-Sequencer-Leg-Id` per outbound INVITE. AC2.
- **Unique per call:** different calls ⇒ different call_id. AC3. (UUID-grade — NFR.)
- **Informational only:** apps/PBX may key on them but need not; they do not replace the
  internal SIP `Call-ID`/tags mapping. AC5.
- **Every outbound hop incl. PBX** carries both. AC4.

## Strategic Approach

#### Solution Direction
- **Mint `leg_id` per leg:** add `newLegID()` (same generator family as `newCallID`).
- **Attach headers at both `Invite` call sites** in `bridge.go` via the existing variadic
  `headers ...sip.Header` parameter:
  `sip.NewHeader("X-Sequencer-Call-Id", call.id)` and
  `sip.NewHeader("X-Sequencer-Leg-Id", newLegID())`.
- Optionally store each leg's `legID` on `OutboundLeg` (handy for later logs/metrics; not
  strictly required by the ACs) — small, decide in canvas.
- **Do not touch** `proxy.go` (unmanaged methods are not calls), the internal SIP
  `Call-ID`/tags, or the call anchor/media.
- **Tests:** fake app + fake PBX capture their received INVITE headers; assert call_id equal
  across all legs, leg_id distinct per leg, both present on every hop incl. PBX, and two
  calls differ.

#### Key Design Decisions
- **D1 — id format: keep 8-byte hex vs switch to UUID (`google/uuid`).**
  PRD §5 says "UUID". `google/uuid` is already an indirect dep. → **Rec: use
  `uuid.NewString()` for both `newCallID` and `newLegID`** (PRD-compliant, UUID-grade
  uniqueness for the NFR). Low risk — `Call.id` stays an opaque string (registry key still
  works). (Alternative: keep hex — fine functionally, but not literally "UUID".)
- **D2 — store leg_id on `OutboundLeg`?** → **Rec: yes, minimal** (`legID string` field) so
  the emitted id is also available for logs/metrics (008) and is greppable; tiny.
- **D3 — header names:** exactly `X-Sequencer-Call-Id` / `X-Sequencer-Leg-Id` (PRD §5),
  informational; no behavior keyed on them by the sequencer.

#### Alternatives Considered
- **Reuse SIP `Call-ID` as the cross-leg id:** rejected — SIP `Call-ID` differs per leg
  (true B2BUA); PRD wants a stable cross-leg handle that is independent of SIP tags.
- **Add headers in a sipgo middleware/hook:** rejected — the two explicit `Invite` call
  sites are clearer and already take headers; YAGNI.

## Risk & Gap Analysis

#### Requirement Ambiguities
- **leg_id on the inbound dialog?** The story says headers on **outbound** leg INVITEs;
  the inbound endpoint dialog is not an outbound INVITE — no header added there. (call_id
  still spans it conceptually via `Call.id`.) Confirm: outbound only.
- **Mid-call / re-INVITE headers (story 007):** not in scope; initial INVITEs only.
- **Pass-through (011) headers:** unmanaged methods are not chain legs — no `X-Sequencer-*`.
  Confirm exclusion.

#### Edge Cases
- **Empty sequence:** only the PBX leg — it still carries both headers (call_id + its
  leg_id). (AC4.)
- **Skipped app (004):** a skipped app's leg was never originated → no header emitted for it
  (correct; it's not a leg). Live legs each carry a distinct leg_id.
- **Tap app legs (010):** these are outbound INVITEs too → they should also carry the headers
  (every outbound leg). Confirm taps included.

#### Technical Risks
- **Header name canonicalization:** ensure sipgo emits the header name verbatim
  (`X-Sequencer-Call-Id`); verify via the fakes. Low risk.
- **uuid switch ripple:** changing `newCallID` to UUID changes the registry-key format only
  (still unique string) — no logic depends on the old format. Keep `Call.id` opaque.
- **Negligible perf** — two header allocations per outbound INVITE.

#### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Stable call_id across all legs | Yes | `call.id` on every outbound INVITE. |
| AC2 | Distinct leg_id per leg | Yes | `newLegID()` per Invite. |
| AC3 | Different calls ⇒ different call_id | Yes | UUID-grade `newCallID`. |
| AC4 | Headers on every outbound hop incl. PBX | Yes | both Invite sites + tap legs. |
| AC5 | Internal SIP mapping unaffected | Yes | `Call-ID`/tags untouched. |
| NFR | UUID-grade uniqueness | Yes | `google/uuid`. |

**Net:** small, localized — add `newLegID()`, optionally a `legID` field on `OutboundLeg`,
attach two informational headers at every outbound `Invite` (app legs incl. taps + PBX),
optionally upgrade ids to UUID. No anchor/media/proxy changes. Decisions for canvas: D1
(UUID vs hex), D2 (store legID on leg), D3 (header names — fixed by PRD). No blockers.
