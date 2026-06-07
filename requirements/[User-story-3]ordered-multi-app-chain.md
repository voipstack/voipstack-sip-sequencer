# [STORY-001-003] Ordered multi-application chain

> Story 003 of the module-001 decomposition of `PRD.md`. See `[User-story-1]` for the
> shared INVEST analysis and split strategy.

### Background
The product's core value is composing several independent SIP services on one call
without coupling them. With the single-application bridge proven, this story generalizes
it to the full ordered sequence: the sequencer walks the YAML `sequence` list in order,
bridging the call through each application in turn, and only after the last application
routes to the terminating PBX. List order is chain order. Each application is unaware of
its position; the sequencer alone owns and enforces the order.

Key points:
- Business value: add/remove/reorder applications by editing config order only.
- Builds directly on the single-app bridge's leg lifecycle.
- Needed now to deliver the actual "sequencer" promise beyond a single hop.

### Business Value
- Provide operators the ability to chain multiple SIP applications on a call in a defined
  order.
- Support add/remove/reorder of call-processing applications via configuration only.
- Enable composition of independent services (e.g. transcription then recording then
  routing) without those services knowing about each other.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-002]` (single-application bridge); `[STORY-001-001]`
  (config).
- **Data assumptions:** All configured application URIs and the PBX next-hop are
  reachable for the happy-path scenarios; failure semantics are covered separately.
- **Integration points:** Multiple external SIP application servers; terminating PBX.
- **Business constraints:** Linear chain only — no branching, no loops; each application
  runs exactly once, in order (PRD §7).

### Scope In
- Walk the configured `sequence` in list order, bridging the call through each
  application before advancing to the next.
- After the last application completes its leg, route the call to the terminating PBX
  next-hop.
- Preserve the inbound-dialog ↔ chain-legs mapping across all hops for the call lifetime.

### Scope Out
- Per-application failure policy (skip/abort) — `[STORY-001-004]`. This story assumes all
  apps succeed (happy path) and may treat any failure as a plain call failure for now.
- RTP anchoring — `[STORY-001-005]`.
- Re-running or re-sequencing the chain mid-call — out of scope (PRD §5).
- Dynamic/per-call ordering — out of scope (PRD §8).

### Acceptance Criteria

#### AC1: Three applications traversed in configured order
**Given** a `sequence` of three applications `[appA, appB, appC]` and a PBX next-hop, all
reachable
**When** a SIP endpoint places a call
**Then** the call is bridged through `appA`, then `appB`, then `appC`, and finally to the
PBX — in exactly that order.

#### AC2: Reordering config changes traversal order
**Given** the same three applications configured as `[appC, appA, appB]`
**When** a SIP endpoint places a call
**Then** the call is bridged through `appC`, then `appA`, then `appB`, then the PBX.

#### AC3: Single-application chain still works
**Given** a `sequence` with exactly one application
**When** a call is placed
**Then** the call is bridged through that one application and then to the PBX (behaviour
identical to the single-app bridge).

#### AC4: Empty sequence routes straight to PBX
**Given** a `sequence` with no applications
**When** a call is placed
**Then** the sequencer routes the call directly to the PBX next-hop with no application
hops.

#### AC5: Applications are unaware of the chain
**Given** a multi-application call in progress
**When** each application's received call is observed
**Then** each application sees an ordinary inbound SIP call and is not asked to forward to
the next application — the sequencer alone advances the chain.

#### AC6: Full chain tears down on hangup
**Given** an established call traversing three applications to the PBX
**When** either end hangs up
**Then** all application legs and the PBX leg are torn down with no dangling legs.

#### Non-Functional Expectations
- Total added setup latency must scale acceptably with chain length for typical chains
  (a few applications) — no per-hop stalls.
