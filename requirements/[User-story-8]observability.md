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
