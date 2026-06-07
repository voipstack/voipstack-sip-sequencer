# SPDD Analysis: Ordered multi-application chain

> Phase 0 (analysis) for `[STORY-001-003]` of the `voipstack-sip-sequencer` module-001
> decomposition. Strategic level. The "How" (slice refactor, exact loop) is left to
> `/spdd-reasons-canvas`.

## Codebase grounding (working notes)
- **Stories 001 and 002 are implemented.** Relevant existing code in `internal/b2bua`:
  - `bridge.go` — `handleInvite` accepts the inbound dialog, sends 100, creates a `Call`,
    runs `bridge` **synchronously** (deliberate: keeps the handler goroutine alive so
    sipgo's `TerminateGracefully` sees a finalized response — documented race fix).
    `bridge` hardcodes **`e.cfg.Sequence[0]`**: originate app leg → `WaitAnswer` (relaying
    18x upstream) → ACK → take app answer SDP → originate PBX leg with that SDP → answer
    endpoint 200 with PBX SDP → `state=established` → block on `<-ctx.Done()`. Failure on a
    leg maps via `mapFailureStatus` (reject ⇒ pass-through status; else ⇒ 503) then
    `teardown`.
  - `call.go` — `Call` has **named** `appLeg`/`pbxLeg *OutboundLeg` fields, a mutex,
    `state`, `cancel`, `reg`. `teardown` is idempotent/glare-safe: snapshots the three
    sessions under lock, cancels ctx, BYEs inbound+appLeg+pbxLeg, removes from registry.
  - `state.go` — pure `mapFailureStatus`, `canTransition`, state enums. `registry.go` —
    mutex map. `engine.go` — UA/server/client, `legTimeout`, dialog caches.
  - Tests in `b2bua_test.go` use real in-memory sipgo fakes (caller/app/PBX), per
    `AGENTS.md` (no internal mocks).
- **Config** (`internal/config`) already parses the **full ordered `Sequence`**; story 002
  only consumed index 0. No config change needed for chaining. (The `media` field is a
  later addition, `[STORY-001-010]`.)
- **Media model:** per the post-§5 decision, the SDP relay in `bridge` is **provisional
  signaling-only scaffolding**; the real media model is anchor + per-app fork
  (`[STORY-001-005]` + `[STORY-001-010]`). This story stays signaling-only and keeps the
  opaque serial SDP relay, now generalized across the chain.

## Original Business Requirement

> Complete `[STORY-001-003]` text, verbatim.

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

## Domain Concept Identification

#### Existing Concepts (from codebase)
- **`Call`** (`call.go`): holds the inbound dialog + legs + lifecycle. Currently models
  **two named legs** (`appLeg`, `pbxLeg`). This story generalizes it to hold an **ordered
  set of application legs** plus the PBX leg.
- **`OutboundLeg`** (`call.go`): one UAC leg with `role`, `targetURI`, `session`,
  `answerSDP`. Reused unchanged; the chain just creates several of them.
- **`bridge` / `handleInvite`** (`bridge.go`): the orchestrator. Generalized from "app[0]
  then PBX" to "each app in order then PBX". The synchronous-handler race fix and the 18x
  relay / failure mapping / answer-timing logic are reused.
- **`teardown`** (`call.go`): generalized to BYE **all** application legs (not just one) +
  PBX leg.
- **`config.Sequence`**: already the full ordered list; now fully consumed.

#### New Concepts Required
- **Chain traversal / leg list:** the in-order walk over `config.Sequence`, accumulating
  one `OutboundLeg` per application. Conceptually the generalization of the two-leg bridge;
  not a new type so much as a new shape for `Call`'s legs (slice instead of two fields).
- **SDP hand-off across hops (provisional):** each app leg is offered the previous hop's
  answer SDP; the last app's answer becomes the PBX offer; the PBX answer is relayed to the
  endpoint. (Signaling-only scaffolding — replaced by anchoring in story 005.)
- **Empty-chain path:** when `Sequence` is empty, the inbound offer goes straight to the
  PBX and its answer back to the endpoint (no app legs).

#### Key Business Rules
- **List order is chain order:** applications are traversed in exactly the configured
  order; reordering config reorders traversal. Governs Chain traversal. AC1/AC2.
- **Each app runs exactly once, in order; linear only:** no branching/looping/re-run.
  Governs Chain traversal. (PRD §7.)
- **PBX is last:** the PBX leg is originated only after the final application completes its
  leg (answered 2xx). Governs Chain traversal. AC1.
- **Empty sequence ⇒ direct to PBX:** zero app hops. Governs Empty-chain path. AC4.
- **Apps unaware of position:** every app receives an ordinary inbound INVITE; the
  sequencer never asks an app to forward. Governs SDP hand-off. AC5.
- **Whole-chain teardown:** a BYE/failure on any leg tears down the inbound dialog and
  **all** application legs and the PBX leg, no dangling legs. Governs `teardown`. AC6.
- **"Completes its leg" = answered 2xx:** an application stays in the (signaling) path; the
  chain advances when it answers, consistent with story 002. Governs Chain traversal.

## Strategic Approach

#### Solution Direction
- **Generalize, don't rewrite.** Replace `Call.appLeg/pbxLeg` with an **ordered slice of
  application legs** (`appLegs []*OutboundLeg`) plus the existing `pbxLeg`; refactor
  `bridge` from the fixed "app[0] then PBX" into a **loop over `e.cfg.Sequence`** that
  originates each app leg, `WaitAnswer`s (relaying 18x), ACKs, and carries the answer SDP
  forward to the next hop. After the loop, originate the PBX leg with the last hop's answer
  SDP (or the inbound offer if the chain is empty), then answer the endpoint with the PBX
  SDP. Keep the synchronous-handler invariant and the failure/answer-timing logic intact.
- **Teardown** iterates the app-leg slice (snapshotting under the mutex as today) and BYEs
  each, plus the PBX leg.
- **Failure handling stays story-002-simple:** any app leg failure fails the whole call
  (pass-through status / 503), exactly as today — per-app skip/abort is `[STORY-001-004]`.
- **Tests** extend the existing real-fake harness to spin up multiple fake app UAS
  instances and assert order, reorder, single, empty, unawareness, and whole-chain
  teardown.
- **Data/signaling flow:** inbound offer → app1 (answer) → app2 offered app1's answer …
  → appN answer → PBX offered appN's answer → PBX answer → endpoint 200. (Provisional SDP;
  anchoring replaces it in 005.)

#### Key Design Decisions
- **Decision: `appLegs []*OutboundLeg` slice vs. keep two named fields.**
  Trade-off: a slice generalizes cleanly to N apps and to teardown iteration vs. minimal
  churn of the existing struct. → Recommend the **slice** (`appLegs`), keep `pbxLeg`
  separate. Rationale: the chain is inherently N-ary; named fields don't scale; PRD §3.
  Touches `call.go`, `bridge.go`, `teardown`, and tests — contained.
- **Decision: how to assert traversal order in tests (AC1/AC2).**
  Trade-off: assert via each fake app observing its INVITE (timestamp/sequence) vs. via
  the `X-Sequencer-*` headers (not until story 006). → Recommend **fake apps record their
  invocation order** (e.g. append to a shared, mutex-guarded slice) and assert the order.
  Rationale: headers aren't available yet; observation at the fakes is direct behavior.
- **Decision: empty-sequence path.**
  → Recommend a clean branch: if `len(cfg.Sequence)==0`, skip the loop and originate the
  PBX leg with the **inbound offer SDP**. Rationale: AC4; avoids special-casing inside the
  loop.
- **Decision: keep failure = fail-whole-call for now.**
  Trade-off: implement skip/abort now vs. defer. → **Defer** to `[STORY-001-004]`; this
  story's scope-out says so and the ACs are happy-path. Avoids scope creep.

#### Alternatives Considered
- **Recursive bridge instead of a loop:** rejected — a flat loop over the slice is simpler
  and matches the linear-chain rule; recursion adds nothing (YAGNI).
- **Per-leg goroutines for parallel origination:** rejected — the chain is **ordered and
  serial** (each hop's answer SDP feeds the next; gatekeeping needs order); parallelism
  would break ordering and SDP hand-off.
- **Generalize media now:** rejected — anchoring/fork are stories 005/010; this stays
  signaling-only.

## Risk & Gap Analysis

#### Requirement Ambiguities
- **Mid-chain failure teardown (happy-path story):** AC6 covers teardown of an
  *established* chain; the failure path (appK fails mid-chain) — tear down apps 1..K-1
  already up — is implied by story 002's behavior but not an explicit AC here. Lean: reuse
  `teardown` to BYE all legs created so far.
- **Latency budget (NFR):** "scale acceptably" / "no per-hop stalls" is unquantified. The
  chain is serial, so setup time ≈ Σ per-hop round trips. For "a few" apps this is fine;
  no hard metric to gate on.
- **Empty-sequence SDP:** confirm PBX is offered the **inbound** offer (not a synthesized
  one) so the endpoint↔PBX negotiation completes. (Decided above.)
- **Provisional media across many hops:** serial SDP relay through N apps is even less
  functional for real audio than 2 hops — but media isn't asserted until 005/010. Just
  ensure signaling completes.

#### Edge Cases
- **Empty sequence** (AC4) — direct to PBX; must not index `Sequence[0]`.
- **Single app** (AC3) — loop of length 1 must behave exactly as story 002 (regression
  guard).
- **Duplicate app `name`** — allowed at config load (deferred validation); chain should
  still run; only matters for metrics/correlation later. Note as boundary.
- **An app answers but the next app/PBX fails** — partial chain must tear down cleanly
  (no leak) — generalization of story 002's behavior.
- **Large chain** — many legs/sessions per call; resource/teardown correctness under
  `-race`.

#### Technical Risks
- **Refactor regression:** changing `Call`'s leg fields touches `teardown` and the bridge;
  risk of breaking story 002 behavior. Mitigation: keep the single-app test passing (AC3
  as regression), small contained change, `go test -race`.
- **Teardown completeness with a slice:** must BYE every app leg (snapshot under mutex);
  risk of leaking a mid-list leg. Mitigation: iterate the snapshot; soak test with chains.
- **Synchronous-handler invariant:** the documented race fix (handler stays alive until a
  final response) must hold across a longer loop; ensure the loop runs within the same
  synchronous `handleInvite` call and still emits exactly one inbound final response.
- **Setup latency growth (NFR):** serial origination adds a round trip per hop; acceptable
  for small chains; flagged, not gated.

#### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Three apps traversed in order | Yes | Loop over Sequence; fakes record order. |
| AC2 | Reorder config ⇒ reorder traversal | Yes | Same; second ordering. |
| AC3 | Single-app chain unchanged | Yes | Regression guard vs story 002. |
| AC4 | Empty sequence ⇒ straight to PBX | Yes | Dedicated branch; no `Sequence[0]` indexing. |
| AC5 | Apps unaware of chain | Yes | Each app gets a normal INVITE; assert at fakes. |
| AC6 | Whole-chain teardown on hangup | Yes | `teardown` iterates app-leg slice + PBX. |
| NFR-latency | Scales acceptably with chain length | Partial | Serial by necessity; sanity-check, not a hard gate. |

**Net:** all ACs addressable by generalizing the existing two-leg bridge to an N-leg loop +
slice, reusing the proven answer-timing/failure/teardown machinery. Load-bearing decisions
for REASONS Canvas: (1) `appLegs` slice refactor shape, (2) empty-sequence branch,
(3) order-assertion approach in tests, (4) confirm failure stays fail-whole-call (skip/
abort deferred to 004). No blockers.
