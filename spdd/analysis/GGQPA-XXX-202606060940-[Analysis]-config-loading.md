# SPDD Analysis: Configuration loading from central YAML

> Phase 0 (analysis) for `[STORY-001-001]` of the `voipstack-sip-sequencer` module-001
> decomposition. Strategic level — "What" & "Why". The "How" (entity fields, function
> signatures, error formats) is left to `/spdd-reasons-canvas`.

## Codebase grounding (working notes)
- **Greenfield repo.** No `go.mod`, no `.go` files, no existing config/YAML files, no
  prior `spdd/` artifacts. Repo currently holds docs only (`PRD.md`, `AGENTS.md`,
  `README.md`, `requirements/`). Dual VCS present (`.git` + `.fslckout`/fossil).
- **Stack (from `PRD.md` §technical objectives):** Go, built on
  [emiago/sipgo](https://github.com/emiago/sipgo), single static binary + systemd.
- **Engineering norms (from `AGENTS.md`):** functional style — pure functions, side
  effects pushed to the edges; Kent Beck simple design + YAGNI; BDD Given/When/Then tests
  named by behavior; mock only external services (none here — filesystem is owned, not an
  external service to stub); `gofmt`/`go vet` clean; errors are values, wrapped with
  `%w`; small consumer-side interfaces; `go test -race` clean.
- **Implication for this story:** config parsing is **pure core** — the ideal first slice
  for a functional design. File read + flag parse + `os.Exit` are the only edges. No
  external service, no DB, no network → no mocks; tests feed bytes/strings directly.

## Original Business Requirement

> The following is the complete `[STORY-001-001]` text, verbatim.

### Background
The sequencer is operated by telecom/VoIP operators who manage it via files and
systemd, not env vars. Before any call can be processed, the process must load a single
central YAML file that fully describes the instance: SIP listen address, terminating
PBX next-hop, RTP port range, and the ordered application sequence. The file is the sole
source of configuration — explicit, versionable, reproducible. This story delivers that
loader: one file in, a validated in-memory configuration out, or a clear startup error.

Key points:
- Business value: one file fully describes an instance → reproducible deployments.
- Foundation for every other story (they all consume this configuration).
- Needed now because no behavior can run without knowing where to listen and route.

### Business Value
- Provide a single, file-based source of truth for operators configuring an instance.
- Support reproducible, version-controlled deployments (the config file is the artifact).
- Enable fast, unambiguous failure when an instance is misconfigured (fail at startup,
  not mid-call).

### Dependencies and Assumptions
- **Prerequisites:** None — this is the foundation story.
- **Data assumptions:** Operator supplies a YAML file at the path given via `--config`.
- **Integration points:** None external; reads the local filesystem only.
- **Business constraints:** No environment variables may influence behavior (PRD §6/§8).
  Only `--config` is accepted on the command line.

### Scope In
- Accept exactly one CLI flag, `--config <path>`, naming the YAML file.
- Parse the YAML into an in-memory configuration: `sip.listen`, `next_hop`,
  `rtp.port_range`, and the ordered `sequence` (each entry: `name`, `uri`, optional
  `on_failure`).
- Apply the `on_failure` default of `skip` when an entry omits it.
- Fail fast at startup with a clear, human-readable error if a required key is missing,
  the file is absent, or the YAML is unparseable.

### Scope Out
- Deep value validation (URI syntax, port-range bounds, duplicate `name`) — malformed
  values surface at use (PRD §6).
- Live reload / SIGHUP — config is read once at startup (PRD §8); change ⇒ restart.
- Any environment-variable or remote-config source (PRD §8).
- Actually listening on the SIP port or processing calls (later stories).

### Acceptance Criteria

#### AC1: Load a complete, valid configuration
**Given** a YAML file containing `sip.listen: 0.0.0.0:5060`, `next_hop: pbx.internal:5060`,
`rtp.port_range: 10000-20000`, and a `sequence` of two applications (`transcribe` with
`on_failure: skip`, `route-guard` with `on_failure: abort`)
**When** the process starts with `--config <that file>`
**Then** startup succeeds and the loaded configuration reflects the listen address, the
next-hop, the port range, and both applications in the exact order listed.

#### AC2: Default failure policy applied when omitted
**Given** a valid YAML file whose `sequence` has one application entry that omits
`on_failure`
**When** the process starts
**Then** that application's failure policy is `skip`.

#### AC3: Missing required key fails fast
**Given** a YAML file that omits `next_hop`
**When** the process starts
**Then** startup fails immediately with an error message naming the missing key
(`next_hop`), and no SIP listener is opened.

#### AC4: Missing config file fails fast
**Given** a `--config` path that does not exist on disk
**When** the process starts
**Then** startup fails immediately with an error stating the file could not be read,
naming the path.

#### AC5: Unparseable YAML fails fast
**Given** a `--config` file whose contents are not valid YAML
**When** the process starts
**Then** startup fails immediately with an error indicating the file could not be parsed.

#### AC6: Environment variables do not influence configuration
**Given** a valid config file and environment variables set that resemble config keys
(e.g. a `NEXT_HOP` env var pointing elsewhere)
**When** the process starts
**Then** the loaded configuration matches the file only; the environment variables have
no effect on listen address, next-hop, port range, or sequence.

#### Non-Functional Expectations
- Startup failure messages must be specific enough for an operator to fix the file
  without reading source code (name the offending key or path).

## Domain Concept Identification

#### Existing Concepts (from codebase)
- None — greenfield. No config type, loader, or domain model exists yet. All concepts
  below are new.

#### New Concepts Required
- **Configuration (instance config):** the in-memory, validated description of one
  sequencer instance — the loader's output. Aggregates the four required pieces. Root of
  the domain; every later story (signaling, chain, media, observability) consumes it.
- **SIP listen endpoint:** where the sequencer accepts inbound SIP — relates to the
  signaling/bridge stories that bind to it.
- **Terminating next-hop (PBX):** the final routing destination after the chain — relates
  to the chain story (last hop).
- **RTP port range:** the media port window the anchor draws from — relates to the media
  anchoring story.
- **Application sequence:** an **ordered** list of application entries; list order is
  chain order. Relates to the chain + failure-handling stories.
- **Application entry:** one chain participant — `name` (identifier for logs/metrics),
  `uri` (next-hop SIP URI), `on_failure` (sequencing policy). Relates to per-app failure
  handling (`on_failure`) and correlation/observability (`name`).
- **Failure policy:** an enumerated sequencing semantic — `skip` | `abort`, defaulting to
  `skip`. Governs the failure-handling story; for this story it is only parsed/defaulted,
  not enforced.
- **Config source (CLI flag `--config`):** the single permitted input channel; the edge
  that turns a path into bytes.

#### Key Business Rules
- **Single source of truth:** configuration comes only from the YAML file named by
  `--config`. Governs Configuration + Config source. No env vars, no remote, no implicit
  defaults file (PRD §6/§8) — an explicit invariant tested by AC6.
- **Required keys present:** `sip.listen`, `next_hop`, `rtp.port_range`, `sequence` must
  all be present; absence ⇒ fail fast (governs Configuration). AC3.
- **Fail fast, fail clear:** any load failure (missing file, unparseable YAML, missing
  key) aborts startup before any side effect (no SIP listener opened), with an
  operator-actionable message. Governs Config source + Configuration. AC3/AC4/AC5 + NFR.
- **Default policy = skip:** an Application entry omitting `on_failure` is `skip`
  (additive sequencing must not endanger calls). Governs Application entry / Failure
  policy. AC2.
- **Order preserved:** the parsed sequence preserves the YAML list order exactly. Governs
  Application sequence. AC1.
- **Shallow validation only (this story):** presence/parse checks only; value-level
  validation (URI syntax, port bounds, duplicate names) is explicitly deferred — malformed
  values surface at use.

## Strategic Approach

#### Solution Direction
- A **pure config package**: `[]byte` (+ a path label for messages) → `(Config, error)`.
  YAML unmarshalling + presence checks + default application live here, with **no I/O** —
  directly unit-testable with inline YAML strings, matching `AGENTS.md` "pure core,
  side-effects at edges."
- A **thin edge in `main`**: parse the single `--config` flag, read the file, call the
  pure loader, and on error print to stderr and exit non-zero (fail fast) before starting
  anything else. This is the only impure part and stays minimal.
- General data flow: `--config flag → read file (edge) → pure parse+validate+default →
  Config value → handed to the rest of the program (out of scope here)`.
- Leverage Go stdlib `flag` for the CLI and a YAML library (sipgo's ecosystem commonly
  pairs with `gopkg.in/yaml.v3`); decode into typed structs. Bootstrap `go.mod` as part
  of this story (greenfield).

#### Key Design Decisions
- **Decision: keep parse pure, isolate file read.**
  Trade-off: a tiny extra seam (caller reads bytes) vs. a loader that does its own I/O.
  → Recommend pure parse. Rationale: testability without temp files, aligns with
  `AGENTS.md`; the file-read edge is trivial and covered at the `main` seam.
- **Decision: presence-only validation; defer value validation.**
  Trade-off: earlier, friendlier errors vs. scope creep + duplicated checks later.
  → Recommend presence-only. Rationale: PRD §6 explicitly defers deep validation; YAGNI.
- **Decision: model `rtp.port_range` as the raw `"10000-20000"` string at load time.**
  Trade-off: parse to (min,max) ints now vs. carry the string and parse where used.
  → Recommend carry as configured/loosely-typed for this story (deep validation is out of
  scope); the media story owns range semantics. Flag as an open question below — minimal
  splitting may still be desirable for an early sanity check. Decide in REASONS Canvas.
- **Decision: `on_failure` as a typed enum with a default applied at load.**
  Trade-off: validate the enum value now vs. accept any string.
  → Recommend applying the `skip` default for the omitted case (required by AC2);
  rejecting unknown values (e.g. `on_failure: explode`) is a value-validation question —
  see open questions.
- **Decision: single `--config` flag, nothing else.**
  Trade-off: future flags vs. strict single-source invariant.
  → Recommend strict single flag. Rationale: PRD invariant; reinforced by AC6.

#### Alternatives Considered
- **Env-var / Viper-style layered config (file + env + flags):** rejected — directly
  violates PRD §6/§8 "no environment variables, single source"; AC6 asserts the opposite.
- **Loader does its own file I/O:** rejected — couples pure logic to the filesystem,
  forces temp-file tests, against `AGENTS.md`.
- **Eager deep validation at load (URI/port/dup-name):** rejected for this story — PRD
  defers it; would duplicate checks the media/chain stories own.

## Risk & Gap Analysis

#### Requirement Ambiguities
- **Unknown / extra YAML keys:** should an unrecognized top-level key be ignored or
  rejected (strict decode)? Strict decoding catches operator typos early (supports the
  fail-clear NFR) but adds rigidity. Not specified.
- **Invalid `on_failure` value:** AC2 covers *omitted*; behavior for an explicit but
  unknown value (e.g. `pause`) is unspecified. Is that "malformed value surfaces at use"
  (deferred) or a load-time error?
- **Empty `sequence`:** is an empty list valid at load? Story 003 AC4 ("empty sequence
  routes straight to PBX") implies **yes** — should be allowed here, not treated as a
  missing key. Worth stating explicitly.
- **"Required key present but empty"** (e.g. `next_hop:` with no value): counts as missing
  or as a deferred malformed value? Lean: treat empty required scalar as missing (AC3
  spirit).
- **Flag absent:** `--config` not provided at all — presumably same fail-fast class as
  AC4; not called out separately.

#### Edge Cases
- Duplicate `name` in sequence — explicitly deferred (out of scope), but the chain/metrics
  stories will care; note as boundary context.
- `port_range` reversed/zero/non-numeric — deferred to media story by PRD, but a reversed
  range is a likely operator error; flagged for REASONS Canvas to decide minimal sanity.
- Very large file / non-UTF8 bytes — read edge should fail cleanly (covered by AC4/AC5
  classes).
- Sequence entry missing `name` or `uri` — are those "required keys" of an entry (fail
  fast) or deferred malformed values? AC3 is about top-level keys; entry-level required
  fields are unstated.

#### Technical Risks
- **Greenfield bootstrap:** `go.mod` init, module path, and YAML dependency selection are
  part of this story; getting the module path right matters for later imports. Low risk,
  but first-mover.
- **Dual VCS (`git` + fossil):** ensure new files land consistently; `.gitignore` exists.
  Low risk for this story (source files), but be aware build artifacts shouldn't be
  committed.
- **Message quality is a tested behavior (NFR):** error wording must name the offending
  key/path — needs `%w`-wrapped, contextual errors, not bare library errors. Drives the
  pure-loader error design.
- **No concurrency/perf concerns** — load-once-at-startup, single-threaded path; `-race`
  trivially satisfied.

#### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Load complete valid config; order preserved | Yes | Pure parse into ordered slice. |
| AC2 | Omitted `on_failure` defaults to `skip` | Yes | Default applied post-decode. |
| AC3 | Missing required key fails fast, names key, no listener | Yes | Presence check; ensure "empty value" interpretation (see ambiguities). |
| AC4 | Missing file fails fast, names path | Yes | File-read edge in `main`. |
| AC5 | Unparseable YAML fails fast | Yes | YAML decode error wrapped. |
| AC6 | Env vars do not influence config | Yes | No env reads anywhere; assertable. |
| NFR | Operator-actionable error messages | Partial | Achievable; quality is judgement — define message contract in REASONS Canvas. |

**Net:** all 6 ACs addressable with the pure-loader + thin-edge approach. Gaps are
ambiguities to resolve in REASONS Canvas (strict-vs-lenient decode, invalid-enum handling,
empty-sequence/empty-value semantics, entry-level required fields), not blockers.
