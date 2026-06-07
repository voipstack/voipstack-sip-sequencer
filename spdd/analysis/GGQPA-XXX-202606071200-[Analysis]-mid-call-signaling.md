# SPDD Analysis: Mid-call signaling (re-INVITE / hold / REFER)

> Phase 0 (analysis) for `[STORY-001-007]` of the `voipstack-sip-sequencer` module-001
> decomposition. Strategic level — "What" and "Why". The "How" (exact handler edits,
> sipgo call shapes) is left to `/spdd-reasons-canvas`.

## Codebase grounding (working notes)
- **Only initial INVITE is handled today.** `engine.go` registers `srv.OnInvite(e.handleInvite)`.
  `handleInvite` (`bridge.go:25`) *always* `dialogSrvCache.ReadInvite` → mints a **new**
  `Call` (`newCallID`) → runs the full `bridge` (loops `e.cfg.Sequence`). There is **no
  branch for an in-dialog re-INVITE** on an already-established dialog. An in-dialog
  re-INVITE/hold would today re-enter `handleInvite` and re-run the whole chain — exactly
  what AC5 forbids.
- **`bridge` is single-shot and linear.** It originates app legs, then the PBX leg, answers
  the endpoint, sets `stateEstablished`, starts `mediaSess.relay`, then **blocks on
  `<-ctx.Done()`** (`bridge.go:469`). After establishment the only live signaling path is
  the `OnState(... DialogStateEnded → teardown)` hooks and the `OnBye`/`OnAck` handlers.
  No mid-call re-offer machinery exists on any leg.
- **REFER is not managed.** `engine.go:107` `srv.OnNoRoute(e.proxyUnmanaged)`. Only
  `INVITE`/`ACK`/`BYE` have explicit handlers; **REFER currently falls through to
  `proxyUnmanaged`** — a stateless forward to `cfg.NextHop` (`proxy.go`). PRD §5 (line 129)
  lists `REFER` among the **managed** call methods, so today's behaviour (blind proxy to
  PBX, no endpoint-leg re-point) does not satisfy AC4.
- **Media addresses are snapshotted at relay start — the central blocker.** `MediaSession`
  (`media.go`) holds `endpointSide`/`pbxSide` `*AnchorSide`, each with a `remoteRTP`/
  `remoteRTCP` `*net.UDPAddr`. `relay` (`media.go:146`) reads those pointers **once** when
  spawning the four copy goroutines (`copyUDPFanout`/`copyUDP` receive `primary`/`dst` by
  value). Mutating `AnchorSide.remoteRTP` after relay starts either has no effect (goroutine
  holds the old value) or data-races the reader (`go test -race` would flag it). Re-anchoring
  on re-INVITE (new endpoint/PBX RTP address or port) therefore needs a synchronized update
  path that does not exist yet.
- **SDP helpers already preserve direction attributes.** `rewriteToAnchor` (`sdp.go:215`)
  rewrites only the first `c=` and first `m=audio` port and passes **all other lines
  verbatim** — so `a=sendonly`/`a=recvonly`/`a=inactive` (the hold/resume signal) propagate
  for free. `parseMedia` (`sdp.go:171`) extracts the new remote host/port from a re-offer.
  These two pure helpers are directly reusable for mid-call.
- **The call audio path is endpoint↔sequencer↔PBX; chain legs are observers.** App legs are
  `media: tap` (recvonly two-`m=` stereo) or `media: none` (`a=inactive`) — they never carry
  call audio (PRD §5). So "propagate the new SDP through the existing chain legs" for
  hold/re-INVITE concerns the **PBX leg** (the live call), not the recvonly tap legs.
- **Correlation ids are minted once.** `X-Sequencer-Call-Id` is stable per `Call` (PRD §5).
  Reissuing it across a transfer's new SIP `Call-ID` is explicitly out of scope (PRD §8).
- `AGENTS.md`: functional core / side-effects at edges, errors as values, mock only external
  services (real in-memory sipgo/RTP fakes, no internal mocks), `gofmt`/`vet`/`-race` clean,
  Kent Beck simple design + YAGNI.

## Original Business Requirement

> Complete `[STORY-001-007]` text, verbatim.

# [STORY-001-007] Mid-call signaling (re-INVITE / hold / REFER)

> Story 007 of the module-001 decomposition of `PRD.md`. See `[User-story-1]` for the
> shared INVEST analysis and split strategy.

### Background
Real calls change after they connect: a party puts the other on hold, codecs renegotiate
(re-INVITE), or a call is transferred (REFER). The PRD fixes clear semantics: re-INVITE
and hold propagate the new SDP through the existing chain legs — the chain is not re-run
mid-call. REFER is handled at the edge — a transfer re-points the endpoint leg while the
chain legs stay. Re-issuing a stable `call_id` across a transfer is out of scope. This
story makes established calls behave correctly under these common mid-call events.

Key points:
- Business value: established calls survive hold, renegotiation, and transfer.
- Builds on the bridge/chain; reuses the existing legs rather than re-sequencing.
- Needed now because production calls routinely hold and transfer.

### Business Value
- Provide correct behaviour for hold/resume and codec renegotiation on live calls.
- Support call transfer (REFER) without disrupting the application chain.
- Enable the sequencer to be deployed inline on real PBX traffic, not just simple calls.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-003]` (chain of legs to propagate through);
  `[STORY-001-005]` (anchored media for SDP changes to apply to).
- **Data assumptions:** Endpoints/PBX issue standard re-INVITE, hold, and REFER.
- **Integration points:** SIP endpoint, applications, PBX.
- **Business constraints:** Chain is not re-sequenced mid-call (PRD §5). REFER is
  edge-only; chain legs stay. No cross-Call-ID `call_id` reissue on transfer (PRD §8).

### Scope In
- On re-INVITE, propagate the new SDP through the existing chain legs (update, do not
  re-run the chain).
- On hold/resume, propagate the hold/resume SDP through the existing legs so media pauses
  and resumes correctly.
- On REFER, handle the transfer at the edge: re-point the endpoint leg while leaving the
  chain legs in place.

### Scope Out
- Re-running or re-sequencing the application chain mid-call — explicitly not done
  (PRD §5).
- Reissuing a stable `call_id` across the transfer's new SIP Call-ID — out of scope
  (PRD §8).
- Branching/conditional handling of mid-call events — out of scope (PRD §8).

### Acceptance Criteria

#### AC1: Hold pauses media through existing legs
**Given** an established call traversing applications to the PBX
**When** the calling endpoint places the call on hold
**Then** the hold is propagated through the existing chain legs so media pauses, without
re-running the chain.

#### AC2: Resume restores media
**Given** a call previously placed on hold
**When** the endpoint resumes the call
**Then** media resumes through the same existing legs.

#### AC3: Re-INVITE updates the existing legs
**Given** an established call
**When** a party issues a re-INVITE with changed SDP
**Then** the new SDP is propagated through the existing chain legs and the same
applications remain in the path — the chain is not re-sequenced.

#### AC4: REFER transfers at the edge, chain legs stay
**Given** an established call traversing applications
**When** a party issues a REFER to transfer the call
**Then** the endpoint leg is re-pointed to the transfer target while the application chain
legs remain in place.

#### AC5: No chain re-run on any mid-call event
**Given** any of hold, resume, re-INVITE, or REFER on an established call
**When** the event is processed
**Then** the applications are not re-invoked and the chain order is not re-evaluated.

#### Non-Functional Expectations
- Mid-call events must apply quickly enough to avoid audible gaps beyond the brief,
  expected pause of hold/resume.

## Domain Concept Identification

#### Existing Concepts (from codebase)
- **Call** (`call.go`): the bridged-call aggregate (inbound dialog, app legs, pbx leg,
  state, media). Mid-call events act on an existing `Call`, found by its SIP dialog —
  they must not mint a new one.
- **CallState** (`state.go`): `setup`/`established`/`tearingDown`. Mid-call events are only
  valid in `established`; this state already gates whether an event is in-dialog.
- **InboundDialog / endpoint leg** (`call.go`): the UAS side. The "endpoint leg" REFER
  re-points and the side that issues hold/re-INVITE toward the sequencer.
- **OutboundLeg (PBX / application)** (`call.go`): UAC legs. The **PBX leg** is the live
  call audio path that a hold/re-INVITE re-offer must propagate to; **app legs** are
  recvonly observers that stay in place untouched (AC3/AC4 "chain legs remain").
- **MediaSession / AnchorSide** (`media.go`): the anchored RTP relay. Re-anchoring updates
  an `AnchorSide`'s remote address; today's relay cannot absorb that change live.
- **rewriteToAnchor / parseMedia** (`sdp.go`): pure SDP helpers, reusable to rewrite the
  re-offer to the anchor and to read the peer's new media address.
- **proxyUnmanaged** (`proxy.go`): the stateless forward REFER currently lands on; this
  story moves REFER off this path into managed handling.

#### New Concepts Required
- **In-dialog event router** — a decision that, on an inbound INVITE, distinguishes an
  *initial* INVITE (run `bridge`) from an *in-dialog re-INVITE* on an established `Call`
  (run mid-call propagation, never the chain). This is the AC5 guard.
- **Mid-call re-offer propagation** — taking the endpoint's new SDP, anchoring it, and
  driving a re-INVITE on the existing PBX leg, then answering the endpoint with the PBX's
  anchored answer. Reuses legs; no `cfg.Sequence` loop.
- **Live media re-anchor** — a synchronized way to swap an `AnchorSide`'s remote RTP/RTCP
  address (and pause/resume direction) while relay goroutines keep running, with no data
  race and no audible gap beyond the hold pause.
- **REFER edge handler** — managed handling of REFER that re-points the endpoint leg toward
  the transfer target while leaving app and PBX chain legs in place; no `call_id` reissue.

#### Key Business Rules
- **No chain re-run (AC5):** any in-dialog event must bypass the `cfg.Sequence` loop and
  leave `appLegs` order and membership unchanged — governs the event router and every path.
- **Reuse existing legs (AC1–AC3):** propagation flows through the already-established PBX
  leg; the app (tap/inactive) legs are not re-originated.
- **Hold/resume is carried in SDP direction attributes:** propagating the verbatim
  `a=sendonly`/`a=inactive`/`a=sendrecv` plus the (possibly `0.0.0.0`) media address is
  what pauses/resumes the relay — governs media re-anchor and SDP rewrite.
- **REFER is edge-only (AC4):** transfer changes the endpoint leg only; chain legs stay; the
  correlation `call_id` is not reissued across the new SIP `Call-ID` (PRD §8).
- **Event validity is state-gated:** mid-call events are only meaningful on an `established`
  `Call`; anything else is not an in-dialog event.

## Strategic Approach

#### Solution Direction
- Split INVITE handling into **initial vs in-dialog**: keep `handleInvite`/`bridge` for the
  first INVITE; route an in-dialog re-INVITE (recognized via the existing dialog/`Call`)
  into a **new mid-call path** that re-anchors media and re-offers only on the PBX leg, then
  answers the endpoint — never entering `cfg.Sequence`. This directly satisfies AC5 and is
  the spine of AC1–AC3.
- **Reuse the pure SDP core.** `rewriteToAnchor` already passes `a=` direction lines
  through, so hold/resume and codec changes ride the same rewrite used at setup; `parseMedia`
  yields the new peer address. The mid-call work is mostly *edge* wiring (dialog re-INVITE,
  socket re-target), keeping the core pure per `AGENTS.md`.
- **Make the relay re-anchorable.** Introduce a synchronized remote-address update on
  `AnchorSide`/`MediaSession` so a re-INVITE can repoint where the relay sends without
  tearing down sockets or the relay goroutines — preserving ports and avoiding audible gaps.
- **Promote REFER to a managed method** with a minimal edge handler that re-points the
  endpoint leg to the transfer target and leaves chain legs intact, rather than the current
  blind `proxyUnmanaged` forward.
- General data flow (re-INVITE): `endpoint re-INVITE → match established Call → anchor new
  SDP → re-INVITE PBX leg → re-anchor media remote addr/direction → answer endpoint`.

#### Key Design Decisions
- **Where to branch initial vs in-dialog INVITE** (in `OnInvite` via dialog lookup, vs a
  separate registered handler): trade-off is coupling in `handleInvite` vs duplicated dialog
  plumbing → **recommend branching inside the INVITE handler** keyed on whether the dialog
  already maps to an established `Call`, keeping one entry point and the AC5 guard in one
  place.
- **Live re-anchor vs relay restart** for media address change: tearing down and restarting
  relay is simpler but risks an audible gap and port churn; a synchronized in-place address
  swap is more code but meets the non-functional "no audible gap" expectation →
  **recommend the in-place synchronized swap** (atomic/mutex-guarded remote address read in
  the copy loops).
- **Do app (tap) legs get re-offered on hold/re-INVITE?** PRD says "propagate through the
  existing chain legs," but tap legs are recvonly observers off the call path → **recommend
  propagating only to the PBX call leg in v1**, treating tap legs as untouched observers, and
  flag the wording for confirmation (see Risks). YAGNI: no tap re-INVITE until a concrete
  need.
- **REFER depth** — blind edge re-point vs full attended-transfer/NOTIFY progress machinery:
  scope names only "re-point the endpoint leg" → **recommend the minimal blind re-point**
  satisfying AC4, no `call_id` reissue, no transfer-progress NOTIFY beyond what the dialog
  layer requires; defer richer transfer semantics.

#### Alternatives Considered
- **Re-run the chain on re-INVITE** (treat it like a fresh call): rejected — directly
  violates AC5 / PRD §5 and would re-originate app legs.
- **Keep REFER on `proxyUnmanaged`** (blind forward to PBX): rejected — it never re-points
  the endpoint leg, so AC4 is unmet; REFER is a managed call method per PRD §5.
- **Tear down and rebuild MediaSession on every re-INVITE:** rejected for the hold/resume
  common case — port churn and a likely audible gap conflict with the non-functional
  expectation; reserve full rebuild only if address family/stream count actually changes.

## Risk & Gap Analysis

#### Requirement Ambiguities
- **"Propagate through the existing chain legs" scope:** chain legs are recvonly/inactive
  observers off the call path. Does AC1/AC3 require re-INVITE-ing each tap/none app leg, or
  only the live PBX call leg? Needs confirmation (recommendation: PBX leg only in v1).
- **Which party issues hold/REFER:** endpoint-initiated is the stated case; PBX-initiated
  re-INVITE/hold and PBX-initiated REFER direction are not spelled out. Clarify whether
  mid-call events must be handled symmetrically on both UAS and UAC sides.
- **UPDATE / PRACK:** PRD §5 lists `UPDATE`/`PRACK` among managed mid-call methods, but this
  story's title and ACs name only re-INVITE/hold/REFER. Confirm UPDATE/PRACK are out of
  scope for 007.
- **REFER transfer style:** blind vs attended transfer, and whether the sequencer must emit
  the `NOTIFY` (`message/sipfrag`) transfer-progress sequence, is unspecified.

#### Edge Cases
- **Hold encodings vary:** `a=sendonly`, `a=inactive`, and the legacy `c=0.0.0.0` all signal
  hold; resume restores `a=sendrecv` and a real address. The re-anchor must handle all three
  and reverse them on resume (AC1/AC2).
- **Codec change on re-INVITE (AC3):** a renegotiated codec must propagate unchanged (no
  transcoding, PRD §5); relay is codec-agnostic byte copy, so this is mainly SDP propagation
  — but verify nothing assumes the setup codec.
- **Glare:** simultaneous re-INVITE from both ends, or a re-INVITE racing a BYE/teardown.
  `Call.mu` + `CallState` must serialize; an in-dialog event arriving during `tearingDown`
  must be rejected cleanly (cf. existing `481` handling for unknown BYE).
- **Re-INVITE with no SDP / unchanged SDP** (session refresh / keepalive re-INVITE): must
  not disrupt media — answer with current anchored SDP, no re-anchor.
- **Address-family or stream-count change** on re-INVITE: in-place swap may be insufficient;
  decide fall-back to rebuild.

#### Technical Risks
- **Relay snapshots remote addresses (`media.go`):** the single biggest risk. Re-anchor
  must update `AnchorSide.remoteRTP/RTCP` under synchronization without racing the running
  copy goroutines (`go test -race`). Mitigation: guard the remote-address read in
  `copyUDPFanout`/`copyUDP` (mutex or atomic pointer) and swap on re-INVITE.
- **`handleInvite` unconditionally mints a new `Call`:** without the initial/in-dialog
  branch, an in-dialog re-INVITE creates a duplicate `Call` and re-runs the chain (breaks
  AC5). Mitigation: dialog-keyed lookup before constructing a `Call`.
- **sipgo capability assumptions:** must verify `dialogCliCache`/`DialogClientSession`
  supports issuing an in-dialog **re-INVITE** on the PBX leg, that `DialogServerCache`
  surfaces an in-dialog re-INVITE distinctly, and that REFER + transfer NOTIFY are
  expressible. If not natively supported, edge plumbing grows. Verify in REASONS Canvas.
- **REFER re-point mechanics:** re-pointing the endpoint leg to a new target while keeping
  the anchored media and chain legs is non-trivial; risk of leaking ports/dialogs if the
  re-point fails mid-way. Reuse `teardown`/release discipline.
- **Non-functional timing:** re-anchor and PBX re-INVITE must complete fast enough to avoid
  gaps beyond the expected hold pause; synchronous round-trips on the PBX leg add latency.

#### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Hold pauses media through existing legs | Partial | Needs in-dialog handler + live media re-anchor (relay snapshot blocker); confirm tap-leg scope. |
| AC2 | Resume restores media | Partial | Same re-anchor path in reverse; must restore `sendrecv` + real address without gap. |
| AC3 | Re-INVITE updates existing legs, no re-sequence | Partial | No in-dialog INVITE branch today; reuse `rewriteToAnchor`/`parseMedia`; PBX-leg re-INVITE machinery new. |
| AC4 | REFER transfers at the edge, chain legs stay | Partial | REFER currently hits `proxyUnmanaged`; needs managed edge re-point; transfer style/NOTIFY ambiguous. |
| AC5 | No chain re-run on any mid-call event | Partial | Requires initial-vs-in-dialog branch so `cfg.Sequence` loop is never re-entered. |

> All ACs are **Partial** because mid-call handling is greenfield: the bridge is single-shot,
> no in-dialog re-INVITE/REFER handling exists, and the media relay cannot yet re-anchor live.
> The pure SDP helpers and `Call`/state/teardown discipline are reusable foundations.
