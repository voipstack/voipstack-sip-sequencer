# [STORY-001-010] Media fork to applications (stereo tap)

> Story 010 of the module-001 decomposition of `PRD.md`. See `[User-story-1]` for the
> shared INVEST analysis and split strategy. Delivers the per-application media fork that
> makes the sequencer's flagship use case (transcription/recording) actually work.

### Background
Applications such as transcription and recording need to **hear the call**. A call has two
audio streams (caller voice and callee voice), but a single SIP leg carries only one
bidirectional stream — so an ordinary leg cannot deliver both directions of the
conversation to an app. This story implements the **fork** described in PRD §5: for an
application configured `media: tap`, the sequencer offers its SIP leg a **recvonly,
two-`m=audio` (stereo) session** and **copies** each call direction into one `m=` line
(caller audio → stream 1, callee audio → stream 2). The application receives **both
directions of the same call** over its single leg, as separate streams — "both legs arrive
as one stereo session." The sequencer does **no mixing and no transcoding**: the fork is a
byte-for-byte duplication of the anchored RTP, split across two `m=` lines. Applications
configured `media: none` are offered audio `inactive` and receive no RTP. This story also
introduces the per-application `media` configuration field.

Key points:
- Business value: media-consuming apps (transcribe, record) finally receive the audio.
- Builds directly on the anchored call (`[STORY-001-005]`): the fork copies those streams.
- Keeps signaling and media orthogonal — every app stays in the signaling chain; `media`
  only controls whether it also receives audio.

### Business Value
- Provide transcription/recording applications both directions of the call as separate
  streams, enabling per-speaker processing.
- Support signaling-only applications (auth, route-guard) that need no media, without
  sending them RTP.
- Enable the product's core promise — media-consuming apps tapping the conversation —
  without any audio alteration.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-005]` (the anchored call provides the two streams to
  copy); `[STORY-001-003]` (apps in the chain); `[STORY-001-001]` (config, extended here).
- **Data assumptions:** tapping apps accept a recvonly two-`m=audio` offer; the codec is
  copied unchanged on the fork; the RTP port range has capacity for fork ports too.
- **Integration points:** external SIP applications (tap and signaling-only).
- **Business constraints:** **no mixing, no transcoding, no resampling** (PRD §5/§8); apps
  never inject audio (recvonly, v1).

### Scope In
- Add a per-application **`media`** config field: `tap` | `none`, default `none` when
  omitted (extends `[STORY-001-001]` config; backward-compatible).
- For `media: tap` applications: offer the app leg a **recvonly two-`m=audio` (stereo)
  session** and **copy** each anchored call direction into one `m=` line (caller →
  stream 1, callee → stream 2), byte-for-byte.
- For `media: none` applications: offer the app leg audio `inactive`; send no RTP; allocate
  no fork.
- Allocate fork ports from the configured `rtp.port_range`; release them on teardown.

### Scope Out
- Mixing the two directions into one stereo stream — explicitly not done (separate `m=`
  lines, no mixing).
- Transcoding / resampling / any audio processing — out of scope (PRD §5/§8).
- Audio injection or modification by apps (announcements, IVR) — out of scope (PRD §8).
- Mid-call media renegotiation on tap legs — `[STORY-001-007]`.
- Deep validation of the `media` value beyond enum membership (consistent with
  `[STORY-001-001]` shallow validation).

### Acceptance Criteria

#### AC1: Tap application receives both call directions
**Given** an application configured `media: tap` in an established call
**When** both the caller and the callee speak
**Then** the application receives two RTP streams over its single leg — one carrying the
caller's audio, one carrying the callee's audio.

#### AC2: Fork is a byte-for-byte copy (no transcoding, no mixing)
**Given** a `media: tap` application on an established call using a negotiated codec
**When** audio flows
**Then** each stream the application receives is byte-for-byte identical to the
corresponding anchored call direction — no codec change, no resampling, and the two
directions are kept separate (not mixed into one stream).

#### AC3: Tap application is recvonly and not in the call path
**Given** a `media: tap` application that sends no audio (or sends audio)
**When** the call is observed
**Then** the call's audio between endpoint and PBX is unaffected by the application — the
application only receives; anything it sends is not relayed into the call.

#### AC4: Signaling-only application receives no media
**Given** an application configured `media: none` (or with `media` omitted)
**When** the call is established and audio flows
**Then** the application is offered audio `inactive` and receives no RTP; the call still
completes normally.

#### AC5: media defaults to none
**Given** an application entry that omits `media`
**When** the configuration is loaded
**Then** that application is treated as `media: none` (no fork).

#### AC6: Fork ports released on teardown
**Given** calls with tapping applications that establish and then end
**When** each call tears down
**Then** the fork media ports are released (no port leak over many calls).

#### Non-Functional Expectations
- The fork must not add audible latency or loss to the call itself under the target load
  (PRD §9) — duplicating packets must not degrade the primary call path.
- A failing or unreachable tap application must not disrupt the call's own media
  (consistent with `on_failure` semantics, `[STORY-001-004]`).