# SPDD Analysis: B2BUA single-application call bridge

> Phase 0 (analysis) for `[STORY-001-002]` of the `voipstack-sip-sequencer` module-001
> decomposition. Strategic level — "What" & "Why". The "How" (dialog APIs, SDP handling
> specifics, function signatures) is left to `/spdd-reasons-canvas`.

## Codebase grounding (working notes)
- **Story 001 is implemented** and is the only existing code:
  - `internal/config/config.go` — `Config{ SIP{Listen}, NextHop, RTP{PortRange},
    Sequence []Application }`, `Application{ Name, URI, OnFailure }`,
    `FailurePolicy` enum (`skip`/`abort`), `Parse([]byte,string)`, `Load(path)`. Pure
    core + thin `Load` edge. Strict YAML decode.
  - `cmd/sip-sequencer/main.go` — parses `--config`, calls `config.Load`, prints a
    one-line "configuration loaded" summary, then returns. **No SIP listener yet** — the
    success path is the explicit placeholder this story replaces.
  - Module path `github.com/voipstack/voipstack-sip-sequencer`, Go 1.23.6.
- **Dependencies:** only `gopkg.in/yaml.v3`. **`emiago/sipgo` is NOT yet a dependency** —
  adding it is part of this story (PRD names it as the SIP stack).
- **Engineering norms (`AGENTS.md`):** functional core / side-effects at edges; Kent Beck
  simple design + YAGNI; BDD Given/When/Then named by behavior; **mock only external
  services** — internal code tested for real; prefer real fakes over mock frameworks;
  `context.Context` for lifecycle on long-lived things; clear goroutine/channel ownership,
  `-race` clean; errors wrapped `%w`; small consumer-side interfaces.
- **Implication:** unlike story 001 (pure), this story is **inherently I/O- and
  concurrency-heavy** (sockets, dialogs, goroutines). The functional-core rule still
  applies where it can — keep call/leg correlation, state transitions, and SDP mapping as
  pure functions over values; push the sipgo sockets/timers to the edges. Tests use **real
  in-memory SIP fakes** (a fake application UAS and a fake PBX UAS built on sipgo), not
  mock frameworks — remote SIP peers are the only "external services" here.

## Original Business Requirement

> Complete `[STORY-001-002]` text, verbatim.

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

## Domain Concept Identification

#### Existing Concepts (from codebase)
- **Config / Application / SIP / RTP** (`internal/config`): supply the listen address,
  the single application `uri`, and the PBX `next_hop`. This story consumes them; it does
  not change them. (`RTP.PortRange` is unused until anchoring, story 005.)
- **`main` startup edge**: currently loads config and stops. This story replaces the
  placeholder success path with "start the B2BUA engine and serve."

#### New Concepts Required
- **B2BUA engine / sequencer service:** the long-lived component that owns the SIP
  listener and the set of active calls. Created from `Config`; started in `main`. Root of
  this story.
- **Call:** the end-to-end unit tying one inbound dialog to its outbound leg(s) for the
  call lifetime. Owns lifecycle/teardown. Reused/extended by chain, mid-call, metrics
  stories. (Conceptual ancestor of the PRD's `call_id`, minted in story 006.)
- **Inbound dialog (endpoint leg):** the terminated UAS side facing the calling endpoint.
- **Outbound leg:** an originated UAC dialog to an application URI or to the PBX. In this
  story a Call has at most two legs over its life (app leg, then PBX leg).
- **Leg mapping / correlation state:** the in-memory association inbound-dialog ↔
  outbound-leg(s) that lets a BYE/teardown on one side tear down the others. Small, owned
  by one component, accessed through a narrow interface (per `AGENTS.md`).
- **Bridge / signaling relay:** the act of connecting two legs — relaying the answer/SDP
  and final responses so the two sides form one call. (Media *anchoring* is story 005;
  here SDP is passed through.)
- **Call lifecycle / state:** setup → established → teardown, plus the failure path
  (app rejects). Drives AC2/AC3/AC4.

#### Key Business Rules
- **True B2BUA, not a proxy:** the inbound dialog and each outbound leg are *separate* SIP
  dialogs with independent Call-ID/tags; the sequencer correlates them internally. Governs
  B2BUA engine, Inbound dialog, Outbound leg. AC5.
- **Sequence is: app first, then PBX:** for a single-entry sequence, originate the app leg;
  on its completion originate the PBX leg; then the call is established end to end. Governs
  Call, Bridge. AC1.
- **Symmetric teardown:** a BYE (or failure) on any leg tears down all other legs of the
  Call, leaving no dangling dialogs/legs. Governs Leg mapping, Call lifecycle. AC2/AC3 +
  NFR (no leak).
- **Fail visibly:** if the application rejects/!=2xx, the call must not reach the PBX and
  the caller must receive a definite failure response (no silent hang). Governs Call
  lifecycle. AC4.
- **Apps unaware of the chain:** the app receives an ordinary inbound INVITE; the sequencer
  does not ask it to forward. Governs Bridge. (Reinforced across stories.)
- **Media passed through (this story):** SDP is relayed between legs; the sequencer does
  not yet own RTP ports. Governs Bridge. (Boundary with story 005.)
- **No state leak:** repeated rejected/failed calls must not accumulate dialog/leg state.
  Governs Leg mapping, Call lifecycle. NFR.

## Strategic Approach

#### Solution Direction
- Add **`emiago/sipgo`** as the SIP stack (PRD-mandated). Use its UA + server/client and
  **dialog** support (UAS dialog session for the inbound side, UAC dialog session for each
  outbound leg) as the B2BUA primitives. Do not hand-roll SIP transactions.
- Introduce a **`b2bua` (or `sequencer`) package** owning: the SIP listener, an active-call
  registry, and the per-call bridging logic. Keep it consumer-side-interface-driven so the
  sockets can be faked in tests.
- **Functional core where possible:** model Call/leg **state and transitions** and any
  SDP/route mapping as pure functions/values; the sipgo sessions, timers, and goroutines
  are the impure edge. This keeps correlation and lifecycle rules unit-testable without a
  network, with full end-to-end behavior covered by real in-memory SIP fakes.
- **Data/signaling flow (single app):** inbound INVITE → UAS accepts/answers per the
  bridged leg → originate UAC leg to `application.uri` → on app 2xx, originate UAC leg to
  `next_hop` (PBX) → relay answer back so endpoint↔PBX is one established call → maintain
  mapping → on any BYE/failure, tear down all legs.
- **Lifecycle via `context.Context`:** engine start/stop and per-call cancellation use
  contexts; each call's goroutines have clear ownership and exit on teardown (`AGENTS.md`).
- Replace `main`'s placeholder: build the engine from `Config`, start it, block until
  signal, shut down cleanly.

#### Key Design Decisions
- **Decision: use sipgo dialog sessions for both sides vs. raw transaction handling.**
  Trade-off: dialog API does CSeq/route/tag bookkeeping for us vs. less low-level control.
  → Recommend dialog sessions. Rationale: less SIP plumbing to get wrong; PRD mandates
  sipgo; YAGNI on custom transaction code.
- **Decision: when to answer the inbound endpoint (early vs. after PBX answers).**
  Trade-off: answer endpoint early (simpler, but media/answer SDP must come from
  downstream) vs. defer the inbound 200 OK until the downstream (app→PBX) path is
  established so the real answer SDP can be relayed. → Recommend **defer inbound 200 OK
  until the bridged far side answers**, relaying its SDP. Rationale: a true B2BUA bridge
  needs the far-side answer to send a correct inbound answer; avoids re-INVITE churn (and
  mid-call re-INVITE is out of scope here). Confirm exact sequencing in REASONS Canvas.
- **Decision: SDP handling without anchoring.**
  Trade-off: relay SDP bodies leg-to-leg unchanged (signaling completes; media path may be
  imperfect until story 005) vs. attempt media correctness now (scope creep into 005).
  → Recommend **relay SDP bodies through, unchanged**, and scope this story to *signaling*
  correctness (which is all the ACs test). Flag media-path correctness as owned by story
  005. (Key ambiguity — see risks.)
- **Decision: active-call registry ownership.**
  Trade-off: a single mutex-guarded map owned by the engine vs. per-call actor goroutines.
  → Recommend a small **engine-owned registry** behind a narrow interface, guarding the
  map; per-call logic in its own goroutine keyed by a call id. Rationale: matches
  `AGENTS.md` "small, owned by one component, narrow interface"; race-free.
- **Decision: test strategy.**
  → Real in-memory SIP fakes (fake app UAS that answers or rejects; fake PBX UAS) plus a
  fake caller UAC, all sipgo, on loopback. Assert connection, teardown, distinct dialogs,
  rejection. No internal mocks (`AGENTS.md`).

#### Alternatives Considered
- **SIP proxy / record-route instead of B2BUA:** rejected — AC5 explicitly requires
  distinct dialogs; PRD requires a B2BUA that anchors media later. A proxy can't anchor.
- **Hand-rolled SIP on `net`:** rejected — PRD mandates sipgo; enormous, error-prone.
- **Anchor media now:** rejected — story 005 owns it; YAGNI/scope.
- **Mock the sipgo layer in tests:** rejected — `AGENTS.md` forbids mocking internal code;
  remote SIP peers are faked instead with real (in-memory) UAS/UAC.

## Risk & Gap Analysis

#### Requirement Ambiguities
- **SDP/media without anchoring:** how exactly is SDP relayed across endpoint↔app↔PBX when
  the sequencer owns no RTP ports? "Passes media negotiation through" is underspecified —
  does media flow end-to-end via relayed SDP, or is media simply not asserted this story?
  Lean: relay SDP bodies; assert *signaling* only; media correctness is story 005. Must be
  nailed down in REASONS Canvas.
- **Inbound answer timing:** spec says app leg completes, *then* PBX leg; when is the
  endpoint's 200 OK sent and with whose SDP? (See design decision; confirm.)
- **"Application completes its leg":** for a single app, does "completion" mean the app
  answered 2xx (call proceeds while app stays in the media path), or the app hung up? PRD
  model implies the app stays in the path (media-consuming), so "completes" = answered 2xx.
  Worth stating explicitly.
- **Provisional responses / ringback:** should 18x from app/PBX be relayed to the endpoint
  (so the caller hears ringing)? Not stated; affects perceived setup behavior (NFR).
- **Failure response mapping (AC4):** which SIP status does the endpoint get when the app
  rejects — the app's status passed through, or a normalized one? Unspecified.
- **Listen transport:** UDP, TCP, or both on `sip.listen`? PRD says plain SIP; default
  likely UDP. Confirm.

#### Edge Cases
- App reachable but never answers (timeout) vs. app actively rejects (4xx) — both must
  produce a clean caller failure and no leak (AC4 + NFR); only rejection is explicit.
- Caller cancels (CANCEL) before the call is established — mid-setup teardown; not called
  out but in the lifecycle.
- PBX rejects after the app answered — call fails after partial setup; the app leg must be
  torn down (generalization of AC3). Unstated for this single-app story.
- Simultaneous BYE from both sides (glare) — teardown must be idempotent / not leak.
- Empty sequence — out of scope here (story 003 AC4), but the engine should not crash if
  handed one; note as boundary.

#### Technical Risks
- **Concurrency correctness:** multiple concurrent calls + per-call goroutines + a shared
  registry ⇒ data-race risk. Mitigation: engine-owned registry behind a narrow interface;
  `go test -race`; clear goroutine ownership per `AGENTS.md`.
- **State/leg leak (NFR):** failed/rejected calls must release dialog sessions, timers, and
  registry entries. Mitigation: `context`-scoped per-call lifecycle + deferred cleanup;
  a test that runs many rejecting calls and asserts zero active calls afterward.
- **sipgo learning/version risk:** dialog API surface and version pinning are new to this
  repo; behavior (auto CSeq, tags) must be understood to satisfy AC5. Mitigation: pin a
  version; thin wrapper; rely on fakes to validate.
- **Testing realism:** building loopback SIP fakes (caller/app/PBX) is non-trivial but
  required (no internal mocks). Mitigation: a small reusable test harness package.
- **Setup latency (NFR):** sequential app-then-PBX origination adds a round trip; must stay
  imperceptible. Low risk for one app; relevant once chained (story 003).

#### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Single-app call completes end to end | Yes | Needs inbound-answer-timing + SDP-relay decisions resolved. |
| AC2 | Caller hangup tears down all legs | Yes | Symmetric teardown via leg mapping. |
| AC3 | Callee (PBX) hangup tears down all legs | Yes | Same mechanism; add "PBX rejects after app answered" edge. |
| AC4 | App rejects ⇒ caller failure, no PBX | Yes | Define status mapping + cover timeout, not just 4xx. |
| AC5 | Distinct inbound/outbound dialogs (true B2BUA) | Yes | Inherent to sipgo dialog sessions; assert separate Call-IDs/tags. |
| NFR-latency | Imperceptible setup latency | Partial | Achievable; not a hard metric — sanity-check, don't gate. |
| NFR-no-leak | No dialog/leg state leak on failure | Yes | Context-scoped cleanup + a many-failures soak test. |

**Net:** all ACs addressable with sipgo dialog sessions + an engine-owned call registry +
real in-memory SIP fakes. The load-bearing open questions for REASONS Canvas are: (1) SDP
relay vs. media scope boundary with story 005, (2) inbound-answer timing/sequencing,
(3) failure status mapping + timeout handling, (4) transport (UDP/TCP) on the listener.
None are blockers; all are design decisions to fix before generation.
