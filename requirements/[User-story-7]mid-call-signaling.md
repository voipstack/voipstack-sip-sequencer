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
