# [STORY-001-009] Deployment (single binary, systemd, release)

> Story 009 of the module-001 decomposition of `PRD.md`. See `[User-story-1]` for the
> shared INVEST analysis and split strategy.

### Background
The sequencer must be easy to deploy and operate on a single host. The PRD calls for a
single static Go binary, a systemd unit for service management, and a simple, repeatable
release mechanism (tagged builds producing the binary). This story delivers the packaging
and release path so operators can install, run, and upgrade the sequencer predictably
without a build toolchain on the target host.

Key points:
- Business value: predictable install/run/upgrade on a single host.
- Independent of call-flow internals; packages whatever the other stories build.
- Needed now so the product is actually shippable and operable.

### Business Value
- Provide operators a single static binary that runs without external runtime
  dependencies.
- Support standard service lifecycle management via systemd (start/stop/restart/enable).
- Enable repeatable, tagged releases that produce the deployable binary.

### Dependencies and Assumptions
- **Prerequisites:** None hard — packages the binary produced by the other stories;
  deliverable in parallel and finalized once a runnable binary exists.
- **Data assumptions:** Target host runs systemd; operator provides a config file at a
  known path (consumed via `--config`, `[STORY-001-001]`).
- **Integration points:** systemd; the release/build pipeline (tagged builds).
- **Business constraints:** Single host, single instance (PRD §8).

### Scope In
- Produce a single static Go binary with no external runtime dependencies.
- Provide a systemd unit that starts the sequencer with `--config` pointing at the
  operator's config file and manages its lifecycle.
- Provide a simple, repeatable release mechanism where a tagged build produces the binary
  artifact.

### Scope Out
- Container images / orchestration manifests — not required (PRD: single host + systemd).
- OS package formats (deb/rpm) — out of scope unless later requested.
- Auto-update / rolling-upgrade orchestration — out of scope.
- Multi-host / clustering — out of scope (PRD §8).

### Acceptance Criteria

#### AC1: Single static binary runs standalone
**Given** a clean target host with no Go toolchain installed
**When** the release binary is copied over and run with `--config <file>`
**Then** the sequencer starts and serves calls without requiring any additional runtime
dependencies.

#### AC2: systemd manages the service lifecycle
**Given** the provided systemd unit installed and pointing at a valid config file
**When** the operator starts, stops, and restarts the service via systemd
**Then** the sequencer starts, stops cleanly, and restarts, and can be enabled to start on
boot.

#### AC3: Tagged build produces the binary
**Given** a tagged release of the source
**When** the release mechanism runs
**Then** it produces the deployable binary artifact for that tag, repeatably (the same tag
yields the same artifact contents).

#### AC4: Bad config surfaces through systemd
**Given** the systemd unit pointing at a config file missing a required key
**When** the service is started
**Then** the service fails to start and the failure (with the configuration error) is
visible in the service status/logs.

#### Non-Functional Expectations
- A clean upgrade (replace binary, restart service) must require only standard systemd
  operations — no bespoke migration steps.
