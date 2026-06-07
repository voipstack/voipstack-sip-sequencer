# SPDD Analysis: Per-application failure handling (skip / abort)

> Phase 0 (analysis) for `[STORY-001-004]` of the `voipstack-sip-sequencer` module-001
> decomposition. Strategic level. The "How" (exact branch edits) is left to
> `/spdd-reasons-canvas`.

## Codebase grounding (working notes)
- **Stories 001/002/003 implemented.** `internal/b2bua/bridge.go` `bridge` loops over
  `e.cfg.Sequence`; for each app it: parses URI → `dialogCliCache.Invite` (originate) →
  appends an `OutboundLeg` to `call.appLegs` → `WaitAnswer` (relaying 18x) → on failure
  **always** `Respond(...)` + `teardown` + return → else ACK, store answer SDP, carry as
  `offer` to the next hop. After the loop, the PBX leg; then answer the endpoint.
- **Two failure branches per app today, both fail the whole call:**
  - originate error (`Invite` err, `bridge.go:76`) → `503` + teardown.
  - answer error (`WaitAnswer` err, `bridge.go:101`) → `mapFailureStatus` (reject ⇒
    pass-through status; timeout/transport ⇒ 503) + teardown.
- **Config already carries the policy.** `config.Application.OnFailure` is a validated
  `FailurePolicy` enum (`skip`/`abort`), **defaulted to `skip` at load** (story 001). So in
  `bridge`, `app.OnFailure` is always exactly `skip` or `abort` — no defaulting needed here.
- **Pure helpers** in `state.go`: `mapFailureStatus`, `canTransition`, `failureKind`.
  `Call`/`teardown` iterate `appLegs` (story 003).
- **No metrics yet.** Prometheus is `[STORY-001-008]`; this story only needs to *emit a
  failure signal* (log + a seam an 008 sink can hook), per AC4.
- `AGENTS.md`: functional core/edges, errors as values, real in-memory sipgo fakes (no
  internal mocks), `-race`.

## Original Business Requirement

> Complete `[STORY-001-004]` text, verbatim.

### Background
Sequencing is additive: adding an optional application to a call must not put calls at
risk if that application is down. The PRD defines a per-application failure policy via
`on_failure`. `skip` means best-effort — on failure the sequencer logs, records a metric,
and advances to the next application. `abort` means required — if it is unreachable or
rejects, the whole call fails. The default when omitted is `skip`, so a dead optional app
never kills calls; gatekeeper apps (auth/route-guard) opt into `abort` explicitly. This
story makes the chain resilient and gives operators control over which apps are critical.

Key points:
- Business value: operators safely add optional apps without endangering live calls.
- Builds on the ordered chain; defines what "completes its leg" vs "fails" means.
- Needed now so real deployments tolerate partial outages.

### Business Value
- Provide operators per-application control over whether an app is required or optional.
- Support resilient calls — an optional app outage degrades gracefully instead of dropping
  calls.
- Enable gatekeeper applications (auth/route-guard) to hard-fail calls when they must.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-003]` (ordered chain); `[STORY-001-001]` (config supplies
  `on_failure`, defaulting to `skip`).
- **Data assumptions:** Failure is observable as an application being unreachable or
  rejecting the leg.
- **Integration points:** Metrics surface (the per-application failure metric is defined
  in `[STORY-001-008]`; this story emits the failure event/log).
- **Business constraints:** Linear chain — a skipped app is simply not run; the chain does
  not retry or reorder.

### Scope In
- On an application leg failure, apply that application's `on_failure` policy:
  - `skip` — log, signal a failure event/metric, advance to the next application.
  - `abort` — fail the whole call.
- Treat an omitted `on_failure` as `skip`.
- A `skip` failure must not drop the call; remaining applications and the PBX hop still
  run.

### Scope Out
- Retries / backoff against a failing application — not in scope (single attempt).
- Branching or alternate sub-chains on failure — out of scope (PRD §8).
- The concrete Prometheus metric definitions — `[STORY-001-008]` (this story emits the
  failure signal that story counts).
- Health of the PBX next-hop hop policy (terminating-hop failure is a plain call failure).

### Acceptance Criteria

#### AC1: skip advances past a failed optional application
**Given** a `sequence` `[appA (skip), appB (skip)]` where `appA` is unreachable and `appB`
and the PBX are reachable
**When** a call is placed
**Then** the sequencer skips `appA`, bridges the call through `appB`, and routes to the
PBX; the call connects successfully.

#### AC2: abort fails the call on a required application
**Given** a `sequence` `[appA (abort), appB (skip)]` where `appA` rejects the leg
**When** a call is placed
**Then** the whole call fails; `appB` and the PBX are not reached, and the calling
endpoint receives a call failure.

#### AC3: Omitted policy defaults to skip
**Given** a `sequence` with one application that omits `on_failure` and is unreachable,
followed by a reachable PBX
**When** a call is placed
**Then** the application is skipped and the call is routed to the PBX (default behaviour
is best-effort).

#### AC4: skip failure is logged and signalled
**Given** an application with `on_failure: skip` that fails during a call
**When** the sequencer skips it
**Then** a failure event is logged identifying the application by its configured `name`,
and a per-application failure signal is emitted for observability.

#### AC5: All-skip chain with every app down still reaches PBX
**Given** a `sequence` where every application is `skip` and all are unreachable, with a
reachable PBX
**When** a call is placed
**Then** every application is skipped and the call still connects to the PBX.

#### AC6: abort on the first app prevents later side effects
**Given** a `sequence` `[appA (abort), appB (skip)]` where `appA` fails
**When** a call is placed
**Then** `appB` never receives a call (no partial chain side effects past the abort).

#### Non-Functional Expectations
- A skipped application must add minimal delay before the chain advances — an optional-app
  outage should not noticeably slow call setup.

## Domain Concept Identification

#### Existing Concepts (from codebase)
- **`bridge` app-chain loop** (`bridge.go`): the two failure branches (originate, answer)
  are exactly where the policy is applied. Today both unconditionally fail the call.
- **`config.Application.OnFailure`** (`config`): the per-app `FailurePolicy` (`skip`/
  `abort`), already validated + defaulted to `skip`. Read here; not changed.
- **`mapFailureStatus`** (`state.go`): still used for the `abort` path's status to the
  endpoint.
- **`teardown`** (`call.go`): still used on `abort` (and only on abort, for app failures).
- **`OutboundLeg` / `call.appLegs`** (`call.go`): the failed app's leg is appended before
  `WaitAnswer`; on `skip` it must not linger as a live leg.

#### New Concepts Required
- **Failure-policy decision point:** at each app failure, branch on `app.OnFailure` —
  `skip` ⇒ log + signal + continue; `abort` ⇒ respond + teardown + return (today's
  behavior, now conditional).
- **Failure signal / metrics seam:** a minimal observability hook (log now; a no-op-default
  sink an 008 Prometheus impl can satisfy) emitting "app `name` failed, action skip/abort".
- **Skip continuation invariant:** on `skip`, the carried `offer` SDP is unchanged (the
  failed app produced no answer), and the failed leg is dropped from `call.appLegs`.

#### Key Business Rules
- **skip = best-effort:** on failure, log + signal + advance; the call is not dropped.
  Governs the app loop. AC1/AC3/AC5.
- **abort = required:** on failure (unreachable or reject), fail the whole call (endpoint
  gets pass-through status / 503) and run no further hops. Governs the app loop. AC2/AC6.
- **Default skip:** consumed from config (already defaulted); additive sequencing must not
  kill calls. AC3.
- **Skip keeps the chain coherent:** a skipped app contributes no SDP; the next hop is
  offered the prior hop's SDP (or the inbound offer if all prior apps skipped). Governs SDP
  hand-off. AC5.
- **No side effects past an abort:** once an abort fails, later apps/PBX are never
  contacted. Governs the loop. AC6.
- **Single attempt:** no retry/backoff/reorder on failure. Governs the loop.
- **Observability on skip:** every skip emits a log (by app `name`) + a failure signal.
  AC4.

## Strategic Approach

#### Solution Direction
- **Localize the change to the two app-failure branches** in `bridge`'s loop. Replace each
  unconditional "respond + teardown + return" with: **if `app.OnFailure == FailureAbort`**
  → today's behavior (respond pass-through/503 + teardown + return); **else (`skip`)** →
  log + emit failure signal + drop the failed leg from `appLegs` + `continue` (leaving
  `offer` unchanged).
- Apply the **same policy to both** the originate-failure branch and the answer-failure
  branch (an unreachable app and a rejecting app are both "the app failed").
- **Failure signal seam:** add a tiny consumer-side interface (e.g. `MetricsSink` with
  `AppFailure(name string)`) on `Engine`, defaulting to a no-op; story 004 calls it on skip
  *and* abort; story 008 provides a Prometheus implementation. (Alternatively: structured
  `slog` only now + counter in 008 — decide in REASONS Canvas.)
- **PBX-leg failure is unchanged** — it is not an application; it always fails the call
  (out of scope per the story).
- **Tests** extend the fake harness: fake apps that reject (4xx) or are unreachable
  (no listener), with `skip`/`abort` configured, asserting advance vs whole-call-fail, the
  all-skip-reaches-PBX case, abort-stops-chain, and that a skip emits the signal.

#### Key Design Decisions
- **Decision: drop the failed leg from `appLegs` on skip vs. add the leg only after a
  successful answer.**
  Trade-off: pop-on-skip is a smaller diff; add-after-answer is cleaner (no dead leg ever
  in the slice). → Recommend **add the app leg to `appLegs` only after it answers**
  (move the append below `WaitAnswer` success). Rationale: keeps `appLegs` = live legs only,
  so `teardown` never BYEs a dead session; removes a class of bug. Small, local.
- **Decision: failure-signal seam now vs. defer all to 008.**
  Trade-off: AC4 explicitly requires a signal emitted on skip. A no-op `MetricsSink`
  interface is minimal and lets 008 plug in. → Recommend the **tiny no-op sink interface**
  (consumer-side, per `AGENTS.md`), called on skip and abort. Keeps AC4 testable now
  (assert the sink was called) without pulling Prometheus into this story.
- **Decision: apply policy to originate-failure too (not just reject).**
  → Yes. "Unreachable or rejects" both count (PRD §7). Both branches honor `OnFailure`.
- **Decision: skip preserves `offer`.**
  → Yes; a failed app has no answer SDP, so the next hop keeps the prior `offer`. No SDP
  synthesis.

#### Alternatives Considered
- **Retry the failed app before skipping:** rejected — single attempt only (scope-out).
- **Tear down and rebuild the chain on skip:** rejected — skip just advances; the prior
  legs stay; linear chain.
- **Put the policy in a pure helper returning an action enum (skip/abort):** worth it —
  a pure `func failureAction(p FailurePolicy) ...` is trivial but the branch is essentially
  `OnFailure == abort`; keep it inline + a one-line comment, or a tiny pure predicate.
  Decide in REASONS Canvas (lean: tiny pure helper for testability/clarity).

## Risk & Gap Analysis

#### Requirement Ambiguities
- **Provisional already sent, then app rejects under skip:** the loop relays the app's 18x
  to the endpoint during `WaitAnswer`. If a `skip` app sends 18x then fails, the endpoint
  has seen provisional responses for an app being skipped. Is that acceptable? For a B2BUA
  the inbound final is still sent later by a successful hop/PBX; interim 18x is harmless but
  worth confirming. Lean: acceptable (provisional, non-final).
- **"emits a metric/signal" granularity (AC4):** does abort also emit the signal, or only
  skip? Story text emphasizes skip; PRD §9 lists "per-application failures" generally. Lean:
  emit on **both** skip and abort (a failure is a failure); decide in canvas.
- **PBX-leg failure metric:** terminating-hop failures are a separate metric (PRD §9 /
  story 008); not an app failure — don't emit the app-failure signal for the PBX leg.

#### Edge Cases
- **All apps skip + all down** (AC5) → PBX offered the inbound offer; must connect.
- **abort app is first** (AC6) → no later contact; verify `appB` fake never invited.
- **Mixed**: `[skip(down), abort(up,reject)]` → skip A, then abort B fails the call.
- **Skip then later abort**: ensure `offer` correctness across a skipped hop feeding a later
  successful/aborting hop.
- **Originate failure vs reject** both honor policy (not just reject).
- **18x from a skipped app** already relayed (see ambiguity).

#### Technical Risks
- **Leg bookkeeping on skip:** if the failed leg is left in `appLegs`, `teardown` later BYEs
  a dead session (noise) or, worse, a half-open one. Mitigation: append-after-answer
  (recommended) so `appLegs` holds only live legs.
- **Regression of story 003 happy path + story 002 single app:** the change is in the
  failure branches; success path must be untouched. Mitigation: existing tests stay green;
  `-race`.
- **Signal seam over-engineering:** keep the sink interface minimal (one method, no-op
  default) to avoid scope creep into 008.

#### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | skip advances past failed app | Yes | `else { log+signal+continue }`. |
| AC2 | abort fails the call | Yes | today's behavior, now under `if abort`. |
| AC3 | omitted ⇒ skip | Yes | config already defaults; no code here. |
| AC4 | skip logged + signalled | Yes | no-op `MetricsSink` seam; assert called. |
| AC5 | all-skip-down still reaches PBX | Yes | offer preserved; loop completes. |
| AC6 | abort stops chain, no later contact | Yes | return on abort; assert later fake idle. |
| NFR | skip adds minimal delay | Partial | skip is immediate on failure; not gated. |

**Net:** a localized change to the two app-failure branches (policy branch on
`app.OnFailure`) + append-after-answer leg bookkeeping + a minimal failure-signal seam.
Load-bearing decisions for REASONS Canvas: (1) append-after-answer vs pop-on-skip,
(2) MetricsSink seam now vs slog-only, (3) emit signal on abort too, (4) tiny pure
`failureAction` helper vs inline. No blockers.
