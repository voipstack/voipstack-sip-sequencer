# [STORY-001-005] RTP media anchoring & relay (the call)

> Story 005 of the module-001 decomposition of `PRD.md`. See `[User-story-1]` for the
> shared INVEST analysis and split strategy. **Scope: the anchored call only** — the
> `endpoint ↔ sequencer ↔ PBX` audio path. Forking media to applications is
> `[STORY-001-010]`.

### Background
The sequencer anchors RTP: it owns the media ports and rewrites the `c=`/`m=` lines of
every SDP it forwards so that all media flows through the sequencer rather than directly
between parties (PRD §5). This story delivers the anchored **call** — the bidirectional
`endpoint ↔ sequencer ↔ PBX` audio path: the sequencer relays the two RTP streams (caller
audio and callee audio) between the endpoint leg and the PBX leg, byte-for-byte. It does
**no transcoding, mixing, or resampling** — it only copies RTP packets. RTP ports come
from the configured port range. This replaces the opaque, non-functional SDP pass-through
of the signaling story (`[STORY-001-002]`) with a real, working media path, and is the
precondition for forking that media to applications.

Key points:
- Business value: a deterministic, sequencer-owned, working media path for every call.
- Pairs with the signaling stories — signaling sets up the legs; this carries the audio.
- Needed before media-consuming apps can be fed (the fork in `[STORY-001-010]` copies
  from this anchored path).

### Business Value
- Provide a deterministic, sequencer-owned media path for every call (no end-to-end RTP).
- Enable a single point of media control without altering the audio (no transcoding).
- Establish the anchored streams that the application fork later duplicates.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-002]` (call legs exist to relay between);
  `[STORY-001-001]` (RTP port range from config).
- **Data assumptions:** parties negotiate a codec the sequencer can relay byte-for-byte;
  the configured RTP port range has enough free ports for the target call volume.
- **Integration points:** remote RTP peers (endpoint, PBX).
- **Business constraints:** **no transcoding/mixing/resampling of any kind** (PRD §5/§8) —
  copy RTP only; plain RTP (no SRTP, v1).

### Scope In
- Anchor RTP for the call: allocate and own the sequencer's media ports for the endpoint
  leg and the PBX leg, rewriting the SDP `c=`/`m=` on each so both parties send RTP to the
  sequencer.
- Relay the two RTP streams (caller audio and callee audio) between the endpoint leg and
  the PBX leg, unchanged (copy only).
- Allocate media ports from the configured `rtp.port_range`; release them on call teardown.

### Scope Out
- **Forking media to application legs (the `media: tap` two-`m=audio` stereo session)** —
  `[STORY-001-010]`. This story carries the call between endpoint and PBX only.
- Transcoding, mixing, resampling, any audio processing — out of scope (PRD §5/§8).
- SRTP / encrypted media — out of scope (PRD §8).
- Mid-call SDP changes (re-INVITE/hold media updates) — `[STORY-001-007]`.

### Acceptance Criteria

#### AC1: Call media flows through the sequencer
**Given** an established call between an endpoint and the PBX
**When** audio is sent from the calling endpoint
**Then** the audio is relayed by the sequencer to the PBX — the media path passes through
the sequencer's own RTP ports, not directly end-to-end.

#### AC2: Media is relayed unchanged (no transcoding)
**Given** an established anchored call
**When** audio is sent in the codec both sides negotiated
**Then** the RTP payload that arrives on the far side is byte-for-byte the same — the
sequencer does not transcode, resample, or mix.

#### AC3: Ports drawn from the configured range
**Given** a configured `rtp.port_range` of `10000-20000`
**When** calls are established
**Then** the media ports the sequencer uses fall within `10000-20000`.

#### AC4: Ports released on teardown
**Given** a series of calls that establish and then end
**When** each call tears down
**Then** the media ports it used are released and become available for later calls (no
port leak over many calls).

#### AC5: Bidirectional audio
**Given** an established anchored call
**When** both parties speak
**Then** both RTP streams (caller audio and callee audio) are relayed in their respective
directions.

#### AC6: Port-range exhaustion fails cleanly
**Given** a `rtp.port_range` already fully allocated to active calls
**When** a new call needs media ports
**Then** the new call fails cleanly (caller gets a definite failure) without disrupting any
established call.

#### Non-Functional Expectations
- Relayed media must not introduce audible latency or loss under the target load of
  100 concurrent calls on a single host (PRD §9).