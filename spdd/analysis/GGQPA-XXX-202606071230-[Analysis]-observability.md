# SPDD Analysis: Observability (Prometheus metrics & health endpoint)

> Phase 0 (analysis) for `[STORY-001-008]` of the `voipstack-sip-sequencer` module-001
> decomposition. Strategic level — "What" and "Why". The "How" (HTTP wiring, exact metric
> types, dependency choice) is left to `/spdd-reasons-canvas`.

## Codebase grounding (working notes)
- **The metrics seam already exists, but is minimal.** `internal/b2bua/metrics.go` defines
  `MetricsSink` with a **single** method `AppFailure(name string)` and a `noopMetrics{}`
  default. `Engine.metrics` (`engine.go:26`) is set to `noopMetrics{}` in `New`
  (`engine.go:77`) and **there is no setter / constructor option to install a real sink** —
  story 008 must add the injection point.
- **`AppFailure` is already emitted at six bridge sites** (`bridge.go:99,112,126,141,187,217`)
  — tap port exhaustion, tap bind failures, app originate failure, app answer failure. So
  AC3 (per-app failure count) only needs the sink method wired to a counter; **no new emit
  points** for failures.
- **No invocation, terminating-hop, or latency emit points exist.** The bridge never calls
  an "app invoked" or "pbx failed" or "sequencing latency" hook today. AC2/AC4/AC5 each need
  a **new emit point** plus a sink method:
  - AC2: a successful app-leg completion (after `appSess.Ack`, `bridge.go:224`) is the
    natural invocation-count site.
  - AC4: the PBX-leg failure branch (`pbxErr != nil`, `bridge.go:399`) is the terminating-hop
    failure site.
  - AC5: time the chain/PBX setup span around `bridge` (start at handler entry, observe at
    endpoint answer, `bridge.go:449`).
- **Active calls already countable.** `Registry.len()` (`registry.go`) returns the live call
  count under lock — the direct source for the active-calls gauge (AC1). Active
  **dialogs/legs** would be derived from each `Call`'s `appLegs`+`pbxLeg`+inbound (a new
  Registry aggregation), definition TBD (see Risks).
- **No HTTP server and no Prometheus dependency.** `cmd/sip-sequencer/main.go` only loads
  config, sets up `slog`, and runs the SIP engine (UDP). `go.mod` has **no
  `prometheus/client_golang`** (deps: sipgo, uuid, yaml). This story introduces both an HTTP
  listener (for `/metrics` + `/health`) and a metrics backend — a new external surface and a
  new dependency decision.
- **No config key for an HTTP/metrics address.** `config.Config` (`config.go`) has `sip`,
  `next_hop`, `rtp`, `sequence`, `log_level` — nothing for an observability listen address or
  path. A new validated key is needed (PRD does not fix the port/path — see Risks).
- **`Application.Name` is the label source.** Validated non-empty at load (`config.go:149`),
  bounded by the config file → safe, low-cardinality Prometheus label.
- **PRD §9** fixes the metric set: active calls, active dialogs/legs, per-application
  invocation count, per-application failures, sequencing latency, terminating-hop failures,
  plus a liveness health endpoint; performance target **100 concurrent calls** (PRD §9).
  Deployment is a **single static Go binary** + systemd (PRD §10).
- `AGENTS.md`: functional core / side-effects at edges, errors as values, **mock only
  external services** (the Prometheus scraper / health prober are external — assert against a
  real in-process HTTP server, not a mock), no internal mocks, `-race` clean, YAGNI.

## Original Business Requirement

> Complete `[STORY-001-008]` text, verbatim.

# [STORY-001-008] Observability (Prometheus metrics & health endpoint)

> Story 008 of the module-001 decomposition of `PRD.md`. See `[User-story-1]` for the
> shared INVEST analysis and split strategy.

### Background
A production inline B2BUA must be observable: operators need to see call volume, chain
health, and per-application behaviour, and orchestration needs a liveness signal. The PRD
specifies Prometheus metrics — active calls, active dialogs/legs, per-application
invocation count, per-application failures, sequencing latency, terminating-hop failures —
and a health endpoint for liveness. This story exposes those so the sequencer can be run
and monitored in production.

Key points:
- Business value: operators can monitor and alert on the sequencer in production.
- Consumes the failure signals (`[STORY-001-004]`) and correlation already in place.
- Needed now for safe production rollout (PRD §9 target: 100 concurrent calls).

### Business Value
- Provide operators real-time visibility into call volume, chain health, and per-app
  behaviour.
- Support alerting on application failures and terminating-hop failures.
- Enable orchestration/monitoring to detect an unhealthy instance via liveness.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-004]` (failure signals to count); `[STORY-001-003]`
  (chain invocations to count). Best delivered after core call flow exists.
- **Data assumptions:** A Prometheus scraper polls the metrics endpoint; an orchestrator
  polls the health endpoint.
- **Integration points:** Prometheus (scrape); liveness probe consumer (e.g. systemd /
  load balancer / k8s).
- **Business constraints:** Single instance; metrics are per-instance (no aggregation).

### Scope In
- Expose a Prometheus metrics endpoint publishing at least: active calls, active
  dialogs/legs, per-application invocation count, per-application failure count,
  sequencing latency, and terminating-hop failure count.
- Expose a health endpoint reporting liveness.
- Increment per-application counters labelled by application `name`.

### Scope Out
- Dashboards / Grafana provisioning — out of scope (PRD §8: no UI/dashboard).
- Distributed tracing or log aggregation backends — not in scope.
- Alerting rules themselves (operators define those against the metrics).
- Persistence of metric history (Prometheus owns retention).

### Acceptance Criteria

#### AC1: Active calls reflected in metrics
**Given** the sequencer with 3 calls established
**When** the metrics endpoint is scraped
**Then** the active-calls metric reads 3, and drops as calls end.

#### AC2: Per-application invocation count
**Given** a `sequence` `[appA, appB]` and 5 calls placed through it successfully
**When** the metrics endpoint is scraped
**Then** the invocation counter for `appA` reads 5 and for `appB` reads 5, labelled by
application name.

#### AC3: Per-application failure count
**Given** an application `appA (skip)` that fails on 2 of 10 calls
**When** the metrics endpoint is scraped
**Then** the failure counter for `appA` reads 2.

#### AC4: Terminating-hop failure count
**Given** a PBX next-hop that rejects 3 calls
**When** the metrics endpoint is scraped
**Then** the terminating-hop failure metric reads 3.

#### AC5: Sequencing latency observed
**Given** calls are placed through the chain
**When** the metrics endpoint is scraped
**Then** a sequencing-latency metric reports observations for those calls.

#### AC6: Health endpoint reports liveness
**Given** a running sequencer
**When** the health endpoint is polled
**Then** it responds indicating the instance is alive; when the process is not running, the
poll fails.

#### Non-Functional Expectations
- Scraping metrics must not measurably degrade call handling at the target load of 100
  concurrent calls.

## Domain Concept Identification

#### Existing Concepts (from codebase)
- **MetricsSink / noopMetrics** (`metrics.go`): the observability seam. Currently only
  `AppFailure(name)`; this story expands it to cover all six PRD metrics and supplies a real
  (Prometheus-backed) implementation behind the same interface.
- **Engine** (`engine.go`): owns `metrics MetricsSink`, the SIP server, and the lifecycle
  (`Run`/`Shutdown`). It must also own/start the metrics+health HTTP listener and expose the
  injection point for a real sink.
- **Registry** (`registry.go`): the live-call store. `len()` is the active-calls source; a
  new aggregation yields active dialogs/legs. Gauges read from it.
- **bridge** (`bridge.go`): the sequencing flow. Already emits `AppFailure`; gains emit
  points for invocation count, terminating-hop failure, and sequencing-latency observation.
- **Application.Name / config** (`config.go`): the per-app metric label and the place a new
  observability listen-address key is added and validated.
- **cmd/sip-sequencer/main.go**: process entry; wires the real metrics sink into the engine
  before `Run` and ties the HTTP listener to process lifecycle.

#### New Concepts Required
- **Metrics registry / collector set** — the concrete Prometheus metrics (gauges for active
  calls and dialogs/legs, counters for per-app invocations, per-app failures, terminating-hop
  failures, a histogram for sequencing latency) behind the `MetricsSink` interface.
- **Observability HTTP endpoint** — an HTTP server exposing `/metrics` (Prometheus exposition)
  and `/health` (liveness), started and stopped with the engine, on a configured address.
- **Liveness signal** — the health concept: a trivial "process is up and serving" response
  (AC6); failure-to-connect when the process is down is the negative signal.
- **Sequencing-latency span** — the measured concept: elapsed time of the per-call sequencing
  work (chain + terminating hop) observed once per call.
- **Active dialogs/legs aggregation** — a derived count over all live `Call`s' legs.

#### Key Business Rules
- **Per-app counters are labelled by application `name`** (AC2/AC3) — governs invocation and
  failure counters; cardinality bounded by config.
- **Active-calls gauge tracks the live lifecycle** — rises on establishment, falls on
  teardown (AC1) — governs the gauge wiring to `Registry`.
- **Invocation counts successful app-leg completion** (AC2 "placed through it successfully");
  failures are a separate counter (AC3) — governs where invocation vs failure is emitted.
- **Terminating-hop failure counts PBX-leg rejections/failures** (AC4) — governs the PBX
  failure emit point, distinct from per-app failures.
- **Scraping must not degrade call handling at 100 concurrent calls** (non-functional) —
  governs lock discipline and keeping the HTTP server off the call-handling goroutines.
- **Metrics are per-instance, liveness is process-level** — no aggregation, no readiness
  semantics beyond "alive".

## Strategic Approach

#### Solution Direction
- **Expand the existing `MetricsSink` interface** to cover the full PRD metric set (app
  invocation, app failure, terminating-hop failure, sequencing-latency observation, plus the
  gauge sources), keep `noopMetrics` as the test/default implementation, and add a **real
  implementation** that records into a metrics backend. This preserves the established seam
  pattern (interface at the consumer, story-008 supplies the impl) and keeps the bridge core
  free of backend specifics.
- **Add an injection point** (constructor option or setter) so `main.go` installs the real
  sink; the engine defaults to `noopMetrics` so existing tests stay backend-free.
- **Run a small HTTP server inside the engine lifecycle** serving `/metrics` and `/health`,
  bound to a new configured address, started in `Run` and stopped in `Shutdown` — on its own
  goroutine, off the SIP/relay path, to honour the 100-concurrent-call non-functional bar.
- **Wire gauges to `Registry`** (active calls via `len()`, dialogs/legs via a new aggregation)
  as collected-on-scrape values rather than push, so they always reflect current state.
- **Add the three missing emit points in `bridge`** (invocation on successful app-leg ACK,
  terminating-hop failure in the PBX-failure branch, latency observation at endpoint answer),
  reusing the existing failure emit sites for AC3.
- General data flow: `bridge events → MetricsSink methods → metrics backend`; and
  `Prometheus scrape → HTTP /metrics → backend exposition (+ Registry-derived gauges)`.

#### Key Design Decisions
- **Prometheus dependency vs hand-rolled exposition:** adding
  `github.com/prometheus/client_golang` is idiomatic, gives histograms/labels/exposition for
  free, and matches PRD §9; the cost is a new module dependency (must be fetched) and binary
  size. Hand-rolling the text format avoids the dep but re-implements histograms and risks
  format bugs. → **Recommend the official client library**; fall back to a minimal hand-rolled
  exposition only if the dependency cannot be vendored/fetched (confirm — see Risks).
- **Where the HTTP server lives:** inside `Engine` (owns lifecycle, can read `Registry`) vs a
  separate component in `main`. → **Recommend inside the engine lifecycle** so it starts/stops
  with `Run`/`Shutdown` and has direct, lock-correct access to `Registry` for gauges.
- **Invocation semantics — attempt vs success:** AC2's scenario is all-success and expects 5.
  → **Recommend counting an invocation on successful app-leg completion** (so
  invocations + failures = attempts), and document it; revisit only if operators need an
  attempt counter.
- **Active dialogs/legs definition:** count outbound legs only, or include the inbound dialog,
  or count "dialogs" and "legs" as two metrics. → **Recommend a single legs gauge =
  inbound + appLegs + pbxLeg across live calls** in v1, name it clearly, and flag the wording
  for confirmation.
- **Config key shape:** a single `observability.listen` (host:port, fixed `/metrics` +
  `/health` paths) vs separate metrics/health addresses. → **Recommend one listen address with
  fixed paths** (YAGNI); validate like `sip.listen`.

#### Alternatives Considered
- **Push metrics (Pushgateway):** rejected — PRD specifies scrape; a single inline instance is
  a natural scrape target.
- **Separate HTTP process/sidecar for metrics:** rejected — over-engineered for a single
  static binary (PRD §10); the in-engine listener is simpler and has direct Registry access.
- **Reuse the SIP UDP port for metrics:** rejected — metrics/health are HTTP/TCP; a dedicated
  HTTP listener is the standard, and keeps scrape traffic off the SIP path.
- **Keep `MetricsSink` at one method and read everything by scraping internal state:**
  rejected — invocation/latency/terminating-hop are events, not state; they need emit points.

## Risk & Gap Analysis

#### Requirement Ambiguities
- **Listen address & paths unspecified:** PRD/story do not fix the metrics/health port or
  URL paths. Needs an operator-facing config key and chosen defaults (e.g. `/metrics`,
  `/health`).
- **"Active dialogs/legs" granularity:** one metric or two? Does it include the inbound
  endpoint dialog and tap legs, or only call-path legs? Confirm the exact definition.
- **Invocation = attempt or success:** AC2 is all-success; the failure/skip interaction
  (does a skipped/failed app still count as "invoked"?) needs confirming. Recommendation:
  success-only.
- **Health semantics:** liveness only (process up) vs readiness (SIP bound, next-hop
  reachable). AC6 reads as pure liveness; confirm no readiness expectation.
- **Latency span boundaries:** chain-only, chain+PBX, or per-app legs? AC5 only requires
  "observations for those calls"; recommendation: one per-call chain+PBX setup span.

#### Edge Cases
- **Apps with duplicate `name`s** in the sequence would merge label series — config does not
  forbid duplicates; decide whether to dedupe/validate or accept summed series.
- **Calls that fail before establishment** (e.g. PBX rejects): must still increment
  terminating-hop failure (AC4) and must not leak the active-calls gauge (decrement on
  teardown even from setup state).
- **`abort`-policy app failure** vs `skip`: both should count as an app failure (AC3); ensure
  every failure branch increments exactly once (no double counting across the six sites).
- **Scrape during high churn:** gauge reads over `Registry` must be lock-correct without
  blocking call handling; histogram/counter writes must be lock-free/atomic.
- **HTTP listener bind failure at startup:** must fail fast and clearly (like SIP listen
  errors), not silently run without observability.
- **Process down (AC6 negative):** the health endpoint cannot respond when the process is
  dead — satisfied inherently by a TCP/HTTP connect failure; no code needed for the negative
  case beyond the server existing.

#### Technical Risks
- **New external dependency:** `prometheus/client_golang` must be added to `go.mod` and
  fetched/vendored. If the build/CI is offline, the dependency cannot be pulled — mitigation:
  confirm module availability, or hand-roll a minimal text exposition (more code, must match
  the Prometheus format precisely).
- **First HTTP surface in the process:** introduces a TCP listener, graceful shutdown, and a
  second lifecycle to manage alongside the SIP server — risk of leaks/races on
  `Run`/`Shutdown`. Mitigation: tie the HTTP server's context to the engine's `runCtx` and
  shut down explicitly.
- **`MetricsSink` expansion touches the bridge:** widening the interface changes every call
  site and `noopMetrics`; risk of an incomplete `noopMetrics` breaking tests. Mitigation:
  keep methods cohesive, update `noopMetrics` to satisfy the full interface.
- **Performance at 100 concurrent calls:** gauge collection that locks `Registry` on every
  scrape, or per-event allocations on the call path, could add latency. Mitigation: minimal
  lock hold for `len()`/aggregation, atomic counters, HTTP off the call goroutines.
- **Label cardinality:** bounded by config app names today; safe, but note that any future
  dynamic labels (e.g. status codes) would need bounding.

#### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Active calls reflected in metrics | Yes | `Registry.len()` exists; wire a gauge that reads it; ensure teardown decrements. |
| AC2 | Per-application invocation count (labelled) | Partial | No invocation emit point today; add one on successful app-leg ACK; new sink method + counter. |
| AC3 | Per-application failure count | Yes | `AppFailure` already emitted at 6 sites; wire to a labelled counter. |
| AC4 | Terminating-hop failure count | Partial | No PBX-failure emit point; add in the PBX-failure branch; new sink method + counter. |
| AC5 | Sequencing latency observed | Partial | No timing today; measure the per-call setup span; new sink method + histogram. |
| AC6 | Health endpoint reports liveness | Partial | No HTTP server today; add `/health` (and `/metrics`) listener in engine lifecycle. |

> AC1 and AC3 are largely **Yes** (sources/emit points already exist). AC2/AC4/AC5/AC6 are
> **Partial** because they each need a new emit point and/or the new HTTP surface and
> dependency. The existing `MetricsSink` seam, `Registry.len()`, and the six `AppFailure`
> sites are the reusable foundations; the central new work is the HTTP endpoint, the
> Prometheus backend, and three bridge emit points.
