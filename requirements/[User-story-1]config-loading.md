# Story Decomposition: voipstack-sip-sequencer

> This file is story **001** of the module-001 decomposition of `PRD.md`.
> Module overview (shared across all 9 stories):

## INVEST Analysis

### Abstract Task
**Feature Name:** SIP application sequencer (B2BUA)

**Analysis Dimensions**
- **Core Responsibility:** Sit inline between SIP endpoints and a PBX; for each call,
  route the dialog through an ordered list of independent external SIP application
  servers (static YAML), anchoring RTP, then on to the PBX.
- **Primary Operations:** load config; accept inbound call; originate per-app legs in
  order; bridge/relay signaling and media; apply per-app failure policy; route to PBX;
  expose metrics/health.
- **Key Constraints:** static linear chain (no branching/loops); apps unaware of chain;
  no transcoding; single PBX/sequence per instance; plain SIP + RTP (no TLS/SRTP v1);
  config from one YAML file only (no env vars).
- **Technical Complexity:** High (B2BUA dialog/leg state, SIP signaling, RTP relay).
- **Business Complexity:** Medium (linear sequence, per-app failure semantics).

### INVEST Evaluation (whole feature)
- ❌ **Independent** — too large to ship as one unit.
- ✅ **Negotiable**
- ✅ **Valuable**
- ❌ **Small** — multi-week.
- ✅ **Testable**

**Conclusion:** Needs splitting.

### Split Strategy
Split **by capability** (not by technical layer). 9 stories:
1. `[STORY-001-001]` Configuration loading from central YAML
2. `[STORY-001-002]` B2BUA single-application call bridge (incoming → one app → PBX)
3. `[STORY-001-003]` Ordered multi-application chain
4. `[STORY-001-004]` Per-application failure handling (skip / abort)
5. `[STORY-001-005]` RTP media anchoring & relay
6. `[STORY-001-006]` Correlation ids & X-Sequencer headers
7. `[STORY-001-007]` Mid-call signaling (re-INVITE / hold / REFER)
8. `[STORY-001-008]` Observability (Prometheus metrics & health endpoint)
9. `[STORY-001-009]` Deployment (single binary, systemd, release)
10. `[STORY-001-010]` Media fork to applications (stereo tap) — added after the §5
    fork-model decision; also extends config with the per-app `media` field.
11. `[STORY-001-011]` Transparent pass-through of unmanaged SIP methods to the PBX —
    added because the sequencer is deployed in front of a PBX (registrar/feature server).

---

## [STORY-001-001] Configuration loading from central YAML

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
- The per-application `media` field (`tap` | `none`) — added later by `[STORY-001-010]`;
  backward-compatible (defaults to `none`). Not part of this story's config schema.
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

---

### Generation summary (module 001)
- Feature: SIP application sequencer (B2BUA)
- Stories generated: 9 (this file = 1/9)
- See companion files `[User-story-2]` … `[User-story-9]` for the rest.
