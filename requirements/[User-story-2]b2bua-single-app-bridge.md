# [STORY-001-002] B2BUA single-application call bridge

> Story 002 of the module-001 decomposition of `PRD.md`. See `[User-story-1]` for the
> shared INVEST analysis and split strategy.
>
> **Media-model note (added after the §5 fork decision).** This story is **signaling
> only** — it asserts dialogs connect, correlate, and tear down; it does not carry working
> audio. Any SDP it relays leg-to-leg is **provisional scaffolding** to complete
> negotiation, *not* the media design. The real media model is **anchor + per-app fork**
> (`[STORY-001-005]` + `[STORY-001-010]`): the call is anchored `endpoint ↔ seq ↔ PBX`,
> and apps receive a recvonly two-`m=audio` fork — apps are **not** serial hops in the
> audio path. The serial "app-answer-SDP → PBX-offer" relay used here is throwaway and is
> replaced by anchoring in `[STORY-001-005]`. The **signaling sequence** (endpoint → app →
> PBX, distinct dialogs, symmetric teardown) is unaffected and remains correct.

### Background
The heart of the product is a back-to-back user agent (B2BUA): it terminates the inbound
SIP dialog from an endpoint and originates a fresh outbound leg, maintaining the mapping
between them for the call's lifetime. Before a multi-application chain has any meaning,
the sequencer must reliably handle the simplest case: one inbound call, bridged through
exactly one external application, then on to the terminating PBX. This story delivers
that minimal end-to-end signaling path. It is the skeleton every later capability hangs
on.

Key points:
- Business value: proves the sequencer can sit inline and complete a real call.
- Establishes dialog/leg state and mapping reused by the chain, mid-call, and metrics
  stories.
- Needed now as the first observable, demoable call flow.

### Business Value
- Provide operators a working inline B2BUA that completes a call through one application.
- Support the core promise — inserting an external SIP service into a call path — at its
  smallest unit.
- Enable every downstream capability (chain, failure, mid-call) to build on a proven leg
  lifecycle.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-001]` (configuration loading) — the listen address,
  next-hop, and a single-entry sequence come from config.
- **Data assumptions:** A reachable external SIP application and a reachable PBX exist at
  the configured URIs; SDP/media is passed through (anchoring is `[STORY-001-005]`).
- **Integration points:** External SIP application server; terminating PBX; SIP endpoint.
- **Business constraints:** Plain SIP only (no TLS); apps remain unaware of the chain.

### Scope In
- Listen for inbound INVITE on the configured SIP listen address.
- Terminate the inbound dialog and originate one outbound leg to the single configured
  application's URI, then, on its completion, originate the leg to the PBX next-hop.
- Maintain the mapping between the inbound dialog and the chain legs for the call
  lifetime.
- Complete normal call setup and teardown (answer, then BYE from either side tears down
  the whole call).

### Scope Out
- More than one application in the sequence — ordered chaining is `[STORY-001-003]`.
- Per-application failure policy semantics — `[STORY-001-004]`.
- RTP anchoring/relay — `[STORY-001-005]` (this story passes media negotiation through).
- Correlation headers — `[STORY-001-006]`.
- Mid-call re-INVITE / hold / REFER — `[STORY-001-007]`.

### Acceptance Criteria

#### AC1: Single-application call completes end to end
**Given** a configuration with one application (`appA`) and a PBX next-hop, and both are
reachable
**When** a SIP endpoint places a call into the sequencer
**Then** the call is bridged through `appA` and then to the PBX, and the endpoint and PBX
are connected as one call.

#### AC2: Caller hangup tears down the whole call
**Given** an established call bridged through `appA` to the PBX
**When** the calling endpoint hangs up
**Then** the sequencer tears down the leg to `appA` and the leg to the PBX, leaving no
dangling legs.

#### AC3: Callee hangup tears down the whole call
**Given** an established call bridged through `appA` to the PBX
**When** the PBX side hangs up
**Then** the sequencer tears down the inbound endpoint dialog and the `appA` leg.

#### AC4: Application rejects the call
**Given** a configuration with one application that rejects the incoming leg
**When** a SIP endpoint places a call
**Then** the call does not connect to the PBX and the calling endpoint receives a call
failure (no silent hang).

#### AC5: Inbound and outbound are distinct dialogs (true B2BUA)
**Given** an established single-application call
**When** the call's signaling is observed on each side
**Then** the inbound endpoint dialog and the outbound legs are separate SIP dialogs that
the sequencer correlates internally — the sequencer is not a transparent proxy.

#### Non-Functional Expectations
- Call setup latency through one application must be low enough not to be perceptible to
  the calling party as an unusual delay.
- A failed or rejected leg must not leak dialog/leg state (no resource buildup over
  repeated failures).
