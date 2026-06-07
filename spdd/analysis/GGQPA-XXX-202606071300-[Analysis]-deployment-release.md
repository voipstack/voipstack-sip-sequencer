# SPDD Analysis: Deployment (single binary, systemd, release)

> Phase 0 (analysis) for `[STORY-001-009]` of the `voipstack-sip-sequencer` module-001
> decomposition. Strategic level — "What" and "Why". The "How" (exact Makefile targets, unit
> directives, CI steps) is left to `/spdd-reasons-canvas`.

## Codebase grounding (working notes)
- **No packaging/release tooling exists yet — greenfield.** There is **no `Makefile`, no
  `Dockerfile`, no `.goreleaser`, no CI workflow** (`.github/`, `.woodpecker`, `.forgejo`,
  `.builds` all absent), and **no systemd unit**. This story creates the build/release/run
  scaffolding from scratch; it adds almost no Go code.
- **The entrypoint already behaves correctly for service management.**
  `cmd/sip-sequencer/main.go`: `--config` is required and **exits `2`** when missing
  (stderr `error: --config is required`); a config load/validation error **exits `1`**
  (stderr `error: <detail>`); logs go to **stderr via `slog`** (journald-friendly); and it
  uses `signal.NotifyContext(ctx, SIGINT, SIGTERM)` → `eng.Run` → `eng.Shutdown`, so a
  systemd stop (SIGTERM) triggers a **graceful shutdown** (BYE all calls). AC2 (clean
  stop/restart) and AC4 (bad config visible) are largely satisfied by this existing
  behaviour — the unit just must not swallow the exit code/stderr.
- **The binary is already pure Go → statically linkable.** `go.mod` deps are sipgo, uuid,
  yaml (and, after `[STORY-001-008]`, `prometheus/client_golang`) — all pure Go, **no cgo**.
  Building with `CGO_ENABLED=0` yields a single static binary with no external runtime deps
  (AC1). No code change needed for static linking.
- **The process is stateless on disk.** It reads one config file (`--config`) and holds only
  in-memory state (calls, RTP ports, metrics). There is no database, no migration, no local
  state directory — so a **clean upgrade is "replace binary + restart"** with no bespoke
  steps (the non-functional expectation) by construction.
- **Config path convention is operator-supplied.** `config.Load(path)` reads whatever
  `--config` points at; PRD §6 shows `--config /etc/voipstack-sip-sequencer/config.yaml` as
  the example. The unit should default `ExecStart` to that conventional path.
- **SCM is ambiguous — git and Fossil both present.** The working tree is a **git** repo
  (branch `last-v1-features`) but also carries **Fossil** artifacts (`.fslckout` SQLite
  checkout DB, `.fossil-settings/ignore-glob`). AC3 ("a tagged release of the source") does
  not say which VCS owns the tag. The release mechanism must either be VCS-agnostic (version
  passed in / derived from either) or this must be clarified (see Risks).
- **Module path:** `github.com/voipstack/voipstack-sip-sequencer`, Go `1.23.6`. There is a second
  binary in the tree (`applications/recording/main.go`) — the deployable is
  `cmd/sip-sequencer`; packaging must target that one explicitly.
- **No version stamping today.** No `-ldflags -X`, no `runtime/debug.ReadBuildInfo` use. A
  tag→version stamp is optional for this story (nice for `--version`/metrics) but not
  required by any AC; reproducibility, not version display, is the AC3 bar.
- `AGENTS.md`: simple design, YAGNI (no deb/rpm/containers — explicitly scope-out), errors as
  values; tests cover behaviour. Packaging artifacts (Makefile/unit) are validated by build +
  a smoke run, not Go unit tests.

## Original Business Requirement

> Complete `[STORY-001-009]` text, verbatim.

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

## Domain Concept Identification

#### Existing Concepts (from codebase)
- **Entrypoint / CLI** (`cmd/sip-sequencer/main.go`): the deployable program. Already
  provides the `--config` flag, the exit-code contract (`2` missing flag, `1` config error,
  `0`/non-zero on run), and stderr logging that systemd/journald capture — the foundation for
  AC2 and AC4.
- **Config loading** (`internal/config/config.go`): `Load(path)` validates required keys and
  returns a descriptive error; this error is what must surface through systemd on AC4.
- **Graceful shutdown** (`Engine.Run`/`Shutdown` driven by `signal.NotifyContext`): the SIGTERM
  handling systemd relies on for a clean stop/restart.
- **Go module** (`go.mod`, pure-Go deps): the source of the statically linkable binary (AC1).

#### New Concepts Required
- **Build recipe** — a repeatable command (Makefile target / build script) that compiles the
  static binary with deterministic flags (`CGO_ENABLED=0`, `-trimpath`, pinned toolchain) for
  the target platform. Governs AC1 and AC3 repeatability.
- **Release mechanism** — the tag-driven process that turns a tagged source state into the
  deployable artifact (a Make `release` target and/or a forge CI pipeline), producing the
  binary (optionally checksummed/named by tag). Governs AC3.
- **systemd unit** — the service definition (`ExecStart` with `--config`, restart policy,
  boot-enable, run-as user, capabilities) that manages lifecycle. Governs AC2/AC4.
- **Install/run documentation** — operator-facing notes (where to put the binary, the config,
  the unit; how to enable/start; how to upgrade) and an example config. Supporting, not an AC,
  but needed for "operable".
- **Version stamp (optional)** — a tag→binary version embedding via `-ldflags -X`; helpful for
  provenance but not required by any AC.

#### Key Business Rules
- **Static, dependency-free binary:** built with `CGO_ENABLED=0`, no external shared
  libraries → runs on a clean host (AC1).
- **Lifecycle via systemd only:** start/stop/restart/enable use standard systemd ops; SIGTERM
  must reach the process for a clean stop (AC2); upgrade = replace binary + `systemctl
  restart`, no migration (non-functional).
- **Repeatable tagged artifact:** the same tag must yield the same artifact contents → the
  build must be deterministic (fixed toolchain, `-trimpath`, no embedded timestamps/paths)
  (AC3).
- **Fail loud on bad config:** a config error must produce a non-zero exit and a journald-
  visible message (AC4) — never a silently-degraded start.
- **Single host / single instance:** no clustering, containers, or OS packages (scope-out).

## Strategic Approach

#### Solution Direction
- **Add a `Makefile` as the single build/release entry point** with deterministic compile
  flags (`CGO_ENABLED=0 go build -trimpath -ldflags "..."`), targeting `cmd/sip-sequencer`,
  output to a `dist/` (or `bin/`) artifact. A `release` target packages the tagged binary
  (named by tag, with a checksum). This is the simplest repeatable mechanism that works on a
  developer host and inside any CI, satisfying AC1 and AC3 without new infrastructure.
- **Provide a systemd unit** (`packaging/systemd/voipstack-sip-sequencer.service`) of
  `Type=simple` (or `notify`), `ExecStart=<bin> --config /etc/voipstack-sip-sequencer/config.yaml`,
  a restart policy, `WantedBy=multi-user.target` for boot-enable, and a dedicated service user
  with the minimum capabilities. Lean on the existing SIGTERM/graceful-shutdown behaviour for
  clean stop/restart (AC2). Because the config error path already exits non-zero to stderr,
  AC4 needs only that the unit not mask it.
- **Keep the release mechanism VCS-agnostic** for now: derive the version from a build arg /
  the tag passed to `make release` rather than hard-coding git or Fossil, given both are
  present in the tree (resolve the source-of-truth in Risks).
- **Document install/upgrade** in a short `packaging/README` (or extend the repo `README`):
  copy binary → place config → install unit → enable/start; upgrade = replace + restart.
- General flow: `tag → make release (deterministic build) → binary artifact → operator copies
  binary + unit + config → systemctl enable/start`.

#### Key Design Decisions
- **Release mechanism shape — Makefile vs CI pipeline:** a Makefile is self-contained,
  runs anywhere, and is the minimal repeatable mechanism; a forge CI pipeline adds automation
  but no CI config exists today and the target forge is unconfirmed. → **Recommend a Makefile
  as the primary mechanism** (AC3 met locally and in any CI), with an optional thin CI job
  that just invokes `make release` on tag — added only if the forge is known.
- **Reproducibility strength — "same tag → same contents":** strict bit-for-bit reproducibility
  needs a pinned Go toolchain version, `-trimpath`, `CGO_ENABLED=0`, stable `-ldflags`, and a
  fixed/`SOURCE_DATE_EPOCH`-style build stamp. → **Recommend `-trimpath` + `CGO_ENABLED=0` +
  pinned Go version + no volatile ldflags** as the practical bar; document that bit-identical
  output also requires the same toolchain version (call this out, don't silently assume).
- **systemd privileges — SIP port binding:** if `sip.listen` uses a privileged port (e.g.
  `:5060`) the service needs `AmbientCapabilities=CAP_NET_BIND_SERVICE` (or root); high ports
  need neither. → **Recommend a non-root `DynamicUser`/dedicated user with
  `AmbientCapabilities=CAP_NET_BIND_SERVICE`** so either port range works, and document it.
- **Target platform matrix:** PRD targets a single Linux host with systemd. → **Recommend
  `linux/amd64` as the primary artifact**, with cross-compile (`GOOS/GOARCH`) trivially
  available for `arm64` if needed; do not build a wide matrix speculatively (YAGNI).
- **Version stamping:** optional `-ldflags -X main.version=<tag>`. → **Recommend including a
  minimal version stamp** (cheap, aids provenance/AC3 artifact naming) but keep it out of any
  behavioural path; no `--version` flag required by the story.

#### Alternatives Considered
- **goreleaser / nfpm:** rejected for v1 — heavier tooling and config than a single-host,
  single-binary, no-OS-package requirement needs (YAGNI); a Makefile suffices.
- **Container image:** rejected — explicitly scope-out (PRD: single host + systemd).
- **deb/rpm packaging:** rejected — explicitly scope-out unless later requested.
- **Embedding the systemd unit / config via `go:embed` and self-installing:** rejected —
  over-engineered; shipping plain unit + example config files is simpler and idiomatic.

## Risk & Gap Analysis

#### Requirement Ambiguities
- **Which VCS tags the release:** git and Fossil both exist in the tree; AC3 "tagged release"
  does not say which. Needs confirmation, or a VCS-agnostic version input.
- **Definition of "repeatable / same artifact contents":** practical determinism
  (`-trimpath`, CGO off, pinned toolchain) vs strict bit-for-bit reproducibility (also fixes
  build timestamps, toolchain version, module hashes). Confirm the required strength.
- **Target OS/arch:** assumed `linux/amd64` + systemd; arm64 or other distros not stated.
- **Config path & file ownership:** the conventional `/etc/voipstack-sip-sequencer/config.yaml`
  and the service user/permissions for reading it are assumed, not specified.
- **Service user & privileges:** run as root vs dedicated user; whether `sip.listen` will use a
  privileged port (affects required capabilities) is config-dependent and unstated.

#### Edge Cases
- **Config file unreadable/missing at the unit's path** (not just missing keys): must also fail
  loudly and visibly (AC4 generalised) — covered by the existing `read config` error path.
- **SIGTERM during active calls:** graceful `Shutdown` BYEs calls; systemd `TimeoutStopSec`
  must allow the 5s shutdown window the code uses, or calls are killed mid-teardown.
- **Restart on crash vs on config error:** `Restart=on-failure` would loop-restart a bad
  config forever; a `StartLimit` / `Restart=on-failure` tuning is needed so AC4 surfaces a
  *failed* unit rather than an endless restart loop.
- **Privileged-port bind without capability:** would fail at SIP listen, surfacing as a service
  start failure — must be documented so operators set the capability.
- **Stale Fossil/git metadata in the artifact:** `-trimpath` and building from a clean checkout
  avoid leaking local paths/VCS state into the binary (reproducibility).
- **Second binary in tree** (`applications/recording`): the build must target only
  `cmd/sip-sequencer` to avoid shipping the wrong artifact.

#### Technical Risks
- **Reproducibility is toolchain-sensitive:** different Go versions produce different binaries;
  "same tag → same contents" only holds with a pinned toolchain. Mitigation: pin the Go version
  (e.g. in the Makefile/CI and document it), use `-trimpath`, avoid volatile ldflags.
- **systemd unit correctness is environment-dependent:** capabilities, user, and stop timeout
  must match the deployment; an incorrect unit fails AC2/AC4 subtly. Mitigation: provide a
  documented, conservative default unit and an install note; verify with a real `systemctl`
  smoke test where possible.
- **No CI exists:** AC3 automation has nowhere to run yet. Mitigation: make the Makefile the
  source of truth so the tagged build is reproducible by hand and trivially CI-able later.
- **Cross-compilation + future cgo deps:** if a future dependency introduces cgo, the static
  guarantee breaks. Mitigation: assert `CGO_ENABLED=0` in the build and keep deps pure-Go.
- **Packaging artifacts are not covered by `go test`:** Makefile/unit bugs won't be caught by
  the Go suite. Mitigation: a documented manual smoke procedure (build → run with good/bad
  config → systemctl start/stop) as the verification for this story.

#### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Single static binary runs standalone | Yes | Build with `CGO_ENABLED=0 -trimpath`; deps already pure-Go; target `cmd/sip-sequencer`. |
| AC2 | systemd manages lifecycle | Yes | New unit; existing SIGTERM/graceful-shutdown supports clean stop/restart; set `TimeoutStopSec` ≥ shutdown window. |
| AC3 | Tagged build produces binary, repeatably | Partial | New Make `release` target; repeatability needs pinned toolchain + `-trimpath`; VCS-for-tag and reproducibility strength to confirm. |
| AC4 | Bad config surfaces through systemd | Yes | Existing non-zero exit + stderr; unit must use `Restart=on-failure` + start-limit so a bad config shows as failed, not looping. |

> AC1/AC2/AC4 are **Yes** — the existing entrypoint (exit codes, stderr logging, graceful
> SIGTERM shutdown) and pure-Go dependency set already provide the behaviour; this story
> supplies the build flags and the systemd unit around them. AC3 is **Partial** pending the
> VCS/tag and reproducibility-strength decisions. The work is almost entirely new packaging
> artifacts (Makefile, `.service`, docs, example config), not Go code.
