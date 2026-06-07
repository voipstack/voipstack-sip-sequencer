# [STORY-001-004] Per-application failure handling (skip / abort)

> Story 004 of the module-001 decomposition of `PRD.md`. See `[User-story-1]` for the
> shared INVEST analysis and split strategy.

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
