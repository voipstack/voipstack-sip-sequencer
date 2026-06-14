# SPDD Analysis: DTLS-SRTP ↔ plain RTP media bridge (no transcoding)

## Original Business Requirement

# Story Decomposition: SIP over WebSocket support (story 5/5)

> Part of the WebSocket decomposition. Module overview and INVEST analysis in
> `[User-story-17]websocket-sip-signaling-transport.md`. Derived from
> `requirements/support-websocket-sip.md`.

---

## [STORY-001-021] DTLS-SRTP ↔ plain RTP media bridge (no transcoding)

### Background
With the webphone's secured media leg up (`[STORY-001-019]` / `[STORY-001-020]`) and
the existing plain-RTP anchor on the other side (`[STORY-001-005]`), media still has
to actually flow between them. This story is the bridge: the anchor **decrypts**
SRTP arriving from the webphone leg and forwards it as **plain RTP** to the opposite
leg, and **encrypts** plain RTP from the opposite leg into SRTP toward the webphone.
This is encrypt/decrypt only — **no transcoding**. Codecs are negotiated end to end
between the two legs, so Opus stays Opus and PCMU stays PCMU; the sequencer never
converts a codec. RTCP is bridged the same way (the rtcp-mux'd webphone side to the
opposite leg). Media failures stay on the media plane — best-effort, isolated to the
affected call.

Key points:
- Business value: audio actually flows end to end between a browser webphone and a
  plain-RTP party — the payoff of the whole WebSocket feature.
- Completes the media plane: leg up (019/020) → packets bridged (this story).
- No transcoding — the bridge only changes the *security* of the media, never the
  codec.

### Business Value
- Provide working two-way audio between a webphone (WebRTC/SRTP) and a plain-RTP
  party through the sequencer's anchor.
- Support every codec the two legs negotiate, with zero conversion cost (no
  transcoding).
- Preserve best-effort media behavior — a media-plane problem affects only the
  call, not the system.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-019]` (secured webphone leg with SRTP keys),
  `[STORY-001-020]` (candidates so the leg connects), `[STORY-001-005]` (the
  plain-RTP anchor on the opposite leg).
- **Data assumptions:** The two legs have negotiated a common codec end to end via
  signaling; the webphone leg's SRTP keys are derived; the opposite leg is plain
  RTP.
- **Integration points:** The browser's WebRTC media and the opposite RTP peer over
  the network.
- **Business constraints:** No transcoding — codecs pass through unchanged. Per-leg
  security is configurable (the bridge reads each leg's security, so an SRTP
  opposite leg can be added later without rework).

### Scope In
- Decrypt SRTP from the webphone leg and forward it as plain RTP on the opposite
  leg.
- Encrypt plain RTP from the opposite leg into SRTP toward the webphone leg.
- Pass the RTP payload through unchanged — no codec conversion in either direction.
- Bridge RTCP between the rtcp-mux'd webphone side and the opposite leg.

### Scope Out
- Bringing the webphone media leg up (ICE-lite, DTLS-SRTP, trickle) —
  `[STORY-001-019]` / `[STORY-001-020]`.
- Transcoding / codec conversion — explicitly never done.
- SRTP on the opposite leg (SRTP↔SRTP) — out of scope now; the bridge reads per-leg
  security so it can be added later without rework.
- TURN relaying — the anchor is the public media path.

### Acceptance Criteria

#### AC1: Two-way Opus audio flows between a webphone and a plain-RTP party
**Given** a webphone leg (DTLS-SRTP) and an opposite plain-RTP leg that have both
negotiated Opus
**When** the call is answered and both sides send media
**Then** audio flows in both directions through the anchor, with the Opus payload
passed through unchanged (no conversion).

#### AC2: A non-Opus codec passes through unchanged
**Given** a call where both legs negotiated PCMU end to end
**When** media flows
**Then** the anchor bridges the PCMU payload unchanged in both directions — still no
transcoding.

#### AC3: SRTP is decrypted and encrypted at the bridge in both directions
**Given** the secured webphone leg and the plain opposite leg
**When** media flows
**Then** packets from the webphone arrive plain on the opposite leg (decrypted), and
packets from the opposite leg arrive encrypted at the webphone (encrypted) — the
security boundary is exactly at the anchor.

#### AC4: RTCP is bridged alongside RTP
**Given** the rtcp-mux'd webphone leg and the opposite leg's RTCP
**When** RTCP reports flow
**Then** they are bridged between the legs so both sides receive their RTCP.

#### AC5: The bridge never converts codecs
**Given** any call whose two legs negotiated a common codec end to end
**When** media is bridged
**Then** the anchor only encrypts/decrypts — it never re-encodes or converts the
codec; if the two ends had not agreed on a codec, that is an end-to-end negotiation
outcome, not something the bridge fixes.

#### AC6: A media-plane failure stays isolated to the call
**Given** an established call whose media bridge encounters a problem (e.g. SRTP
auth failure on one leg)
**When** the failure occurs
**Then** it is handled best-effort and confined to that call, without affecting
other calls or the signaling plane.

#### Non-Functional Expectations
- The bridge's only per-packet work is encrypt/decrypt and forwarding — it must add
  no codec-processing (transcoding) cost.
- Bridging must be structured so that making the opposite leg SRTP later
  (SRTP↔SRTP) requires no change to the bridge's forwarding path — only that leg's
  security property.

## Domain Concept Identification

### Existing Concepts (from codebase)

- **MediaLeg** (`internal/b2bua/mediasec.go`): the per-leg abstraction over security
  profile. Today it exposes only `Security()`, `ReadRTP(buf)` (yields *decrypted*
  RTP regardless of wire security) and `Close()`. It is the intended uniform seam
  for the bridge but is currently **read-only** — no plaintext-write/encrypt path
  exists. — relates to both `AnchorSide` and `SecuredLeg`.
- **MediaSecurity** (`mediasec.go`): per-leg enum `SecurityPlainRTP` /
  `SecurityDTLSSRTP`. Documented as the lever that lets SRTP↔SRTP be added later by
  flipping the opposite leg's property — directly governs the non-functional
  "no forwarding-path change" expectation.
- **AnchorSide** (`media.go`): a plain RTP/RTCP UDP endpoint with atomically-swappable
  remote addresses. Satisfies `MediaLeg` with `Security()==SecurityPlainRTP` and a
  direct socket-read `ReadRTP`. It is the plain (opposite / PBX) leg of the bridge.
- **SecuredLeg** (`webrtc.go`): the DTLS-SRTP leg, delegating to a `WebRTCEndpoint`.
  Satisfies `MediaLeg` via the endpoint's decrypted `ReadRTP`. Carries the SDP answer
  and the trickle-candidate delegation. This is the secured (webphone) leg.
- **WebRTCEndpoint** (`webrtc.go`): the swappable media-library boundary (pion is the
  only implementor). Exposes `Answer`, `ReadRTP` (decrypted), `LocalPort`,
  trickle-candidate methods, `Close`. Has **no write/encrypt-outbound seam** today —
  the `OnTrack` handler captures an inbound `TrackRemote` only; no local track is
  added for outbound SRTP.
- **MediaSession** (`media.go`): owns the two sides and the relay. Today it relays
  *plain↔plain* only: `relay()` runs four `copyUDP*` goroutines over raw `*net.UDPConn`
  of `endpointSide` and `pbxSide`. When `endpointLeg` (secured) is set, `endpointSide`
  is nil and **no relay runs** — explicitly deferred to this story (comment at
  `media.go:144-147`).
- **Tap / media fork** (`media.go`, `[STORY-001-007]`): RTP fan-out to applications.
  Taps consume *plaintext* RTP on the caller/callee directions; whatever bridge path
  is built must preserve the fan-out point so transcription etc. still sees plaintext.
- **mediaAnchor / Engine.runBridge** (`bridge.go`): the orchestration. At
  `bridge.go:128` the secured-leg branch answers the webphone and then just waits on
  `ctx.Done()` — it never dials the PBX nor starts a relay. The plain path
  (`bridge.go:136-140`) dials the PBX and runs `relay()`.

### New Concepts Required

- **Plaintext write/encrypt seam on a media leg** — a counterpart to `ReadRTP` that
  accepts plaintext RTP and emits it on the leg with the leg's own security
  (plain → raw socket write; secured → pion encrypts to SRTP). This is the missing
  half of `MediaLeg`; it makes encrypt/decrypt a property of the leg, not of the
  relay, which is what keeps SRTP↔SRTP a future config change rather than a rewrite.
- **Outbound track on the WebRTC endpoint** — a local media track so pion encrypts
  plaintext RTP toward the webphone. Conceptually "the webphone leg can be written
  to", paired with the existing "can be read from".
- **Security-agnostic bridge relay** — a relay that moves plaintext between the two
  `MediaLeg`s via read/write seams (and fans out to taps), replacing the raw-UDP-only
  `relay()` for the secured-endpoint case. Same forwarding logic regardless of which
  legs are secured.
- **RTCP bridging across the rtcp-mux'd secured leg** — an RTCP read/write seam on the
  secured leg (single muxed port) tied to the plain leg's separate RTCP socket.

### Key Business Rules

- **Security boundary is exactly at the anchor** (AC3): inbound is decrypted before it
  leaves toward the opposite leg; outbound is encrypted as it enters the webphone leg.
  Governs `MediaLeg` read (decrypt) + the new write (encrypt) seams.
- **Never transcode** (AC1/AC2/AC5): the bridge moves RTP payload bytes verbatim;
  codec is an end-to-end signaling outcome. Governs the relay — it must not touch the
  payload, only security framing. Also a non-functional cost constraint (no codec CPU).
- **Per-leg security independence** (non-functional): the forwarding path reads each
  leg's `MediaSecurity` and must not hard-code "webphone=SRTP, other=RTP". Governs the
  relay/leg abstraction so SRTP↔SRTP needs only a leg property change.
- **Media failures are best-effort and call-isolated** (AC6): a decrypt/auth/socket
  error on one leg tears down or degrades only that call's media plane, never other
  calls nor signaling. Governs error handling in the relay goroutines.
- **RTCP follows RTP** (AC4): RTCP is bridged with the same security transform across
  the rtcp-mux'd boundary.

## Strategic Approach

### Solution Direction

- Complete the deferred secured-endpoint path: for a webphone (WebRTC) call, after the
  secured leg is answered, **dial the PBX and bring up the plain `pbxSide` anchor**
  (the existing `[STORY-001-005]` machinery), then run a relay that bridges the
  secured `endpointLeg` to the plain `pbxSide`.
- Make encrypt/decrypt a **property of each leg**, not of the relay. Extend the
  `MediaLeg` abstraction with a plaintext **write** seam to mirror the existing
  plaintext **read** seam. `ReadRTP` already yields decrypted RTP; the new write seam
  accepts plaintext and applies the leg's outbound security. The relay then only ever
  moves plaintext between two `MediaLeg`s and never knows about SRTP. This is what
  satisfies the non-functional "SRTP↔SRTP later, no forwarding-path change" rule.
- On the pion `WebRTCEndpoint`, add a **local track** so outbound plaintext RTP is
  encrypted into SRTP toward the webphone, and expose the inbound RTCP/outbound RTCP
  on the same muxed port. The plain `AnchorSide` write seam is a direct UDP write to
  the swappable remote.
- Replace the secured case's relay with a **leg-oriented relay** that reads plaintext
  from one leg and writes plaintext to the other (both directions, RTP + RTCP), fanning
  out to taps with the existing plaintext fan-out. General data flow:
  `secured leg ReadRTP (decrypt) → pbxSide write (plain) → opposite peer` and
  `pbxSide read (plain) → secured leg WriteRTP (encrypt) → webphone`.
- Keep error handling identical to the current relay: log, stop the affected
  direction, let teardown release sockets — confined to the one call.

### Key Design Decisions

- **Where the encrypt/decrypt lives** — in the leg (via `MediaLeg`) vs. in the relay.
  Trade-off: putting it in the relay is a smaller immediate diff but bakes in the
  "one secured + one plain" assumption and violates the non-functional rule;
  putting it in the leg adds a write method to two types now but makes the relay
  security-agnostic. → **Recommend in the leg** — the requirement's non-functional
  expectation and the existing `MediaSecurity`/`MediaLeg` design both point here.
- **One relay or two** — extend `relay()` to handle both plain↔plain and leg↔leg, vs.
  a separate leg-oriented relay for the secured case. Trade-off: a single unified
  relay over `MediaLeg` read/write seams is the DRY end state, but the plain path
  currently relies on raw `*net.UDPConn` copy and tap fan-out semantics that are
  socket-shaped. → **Recommend converging on a `MediaLeg`-based relay**, but the
  decision of whether to also migrate the existing plain↔plain path or only add the
  secured path is a REASONS-Canvas-level scoping call; the safe minimum is a
  leg-oriented relay for the secured case that reuses the plaintext tap fan-out.
- **RTP buffer / packet handling** — reuse the existing `rtpBufSize` (1500) copy loop
  shape and per-packet atomic remote-address load for the plain side, so the secured
  bridge behaves like the proven plain relay. → **Recommend reuse** for consistency.
- **PBX dial for webphone calls** — the secured branch must now invoke the same
  `dialPBX` flow the plain branch uses. → **Recommend reusing `dialPBX`/`anchorMedia`
  building blocks** rather than a parallel path, to avoid duplicating call setup.

### Alternatives Considered

- **Transcoding / re-encoding in the anchor** — rejected outright; the requirement
  forbids it (AC5) and it adds CPU cost the non-functional section bans.
- **Encrypt/decrypt in the relay goroutines (leg stays read-only)** — rejected; it
  hard-codes the secured/plain asymmetry and breaks the SRTP↔SRTP-without-rework
  requirement.
- **Bridging raw SRTP without decrypting (forward ciphertext)** — impossible: the two
  legs have independent keying (DTLS-derived on one side, none on the other); the
  anchor must be the cryptographic boundary (AC3).

## Risk & Gap Analysis

### Requirement Ambiguities

- **Outbound SRTP keying / track setup**: the story says "encrypt plain RTP into SRTP
  toward the webphone" but the current pion endpoint only captures an inbound track;
  whether a local track with a matching payload type / SSRC must be pre-negotiated in
  the `Answer` SDP (sendrecv vs. recvonly) is unspecified. Needs resolution in the
  canvas — the answer SDP from `[STORY-001-019]` must advertise a sendable track for
  outbound media to flow.
- **Codec/payload-type alignment**: "codecs negotiated end to end" assumes the
  webphone's negotiated PT equals the PBX side's PT. If the two legs negotiated
  different payload-type numbers for the same codec, forwarding raw bytes is wrong
  unless PTs match. The story treats codec as truly end-to-end (same PT both sides);
  this assumption should be made explicit.
- **"Bridge reads each leg's security"**: how the relay branches on `MediaSecurity`
  vs. relying purely on polymorphic `MediaLeg` read/write is left open — the
  non-functional rule implies polymorphism, not a `switch`.

### Edge Cases

- **DTLS / ICE not yet complete when PBX answers**: `ReadRTP` blocks on `e.ready`
  (track arrival). The bridge must tolerate one leg being live before the other and
  not spin or drop the call.
- **SRTP auth/replay failure mid-call** (AC6 example): must degrade that call only.
  Current `copyUDP` logs and returns on write error; the secured read path must do the
  same on decrypt error.
- **rtcp-mux on the webphone vs. separate RTP/RTCP ports on the plain side**: the
  plain `AnchorSide` has distinct RTP/RTCP sockets; the secured leg muxes both on one
  port. The RTCP bridge must reconcile these two shapes.
- **Hold / re-anchor / REFER mid-call**: `reanchor`, `midcall.go`, `refer.go` all read
  `media.endpointSide` (the plain anchor). For a webphone call `endpointSide` is nil
  (`endpointLeg` is set) — mid-call signaling on the secured side may panic or no-op.
  This is a boundary risk to surface even if full mid-call support is out of scope.
- **Taps on a secured call**: taps consume plaintext; the fan-out must occur on the
  decrypted stream, and tap write must not block the primary bridge direction.
- **Endpoint closed before track ready**: `ReadRTP` returns an error; relay must treat
  it as normal teardown, not an error storm.

### Technical Risks

- **pion outbound encryption seam**: adding a local `TrackLocalStaticRTP` (or
  equivalent) and writing pre-formed RTP through it so pion encrypts — getting SSRC /
  payload-type / clock-rate right is the main implementation risk. Mitigation: keep the
  pion specifics behind `WebRTCEndpoint`; test the leg behavior, not pion internals
  (AGENTS.md: mock external services only, the WebRTC peer is the external boundary).
- **Concurrency / ownership**: new write seams introduce a second writer to a leg
  (relay + possibly RTCP); each goroutine must own its direction with no shared mutable
  state, `go test -race` clean (AGENTS.md).
- **Testability without a real browser**: the bridge behavior must be provable with a
  fake `WebRTCEndpoint` (already the test pattern in `webrtc_test.go`) that records
  written plaintext and yields canned decrypted RTP — so AC1/AC2/AC3/AC5 are
  assertable without DTLS/ICE on the wire. Risk: tests that assert pion internals
  rather than bridge behavior — avoid.
- **mid-call code paths assuming `endpointSide`**: extending the bridge may require
  guarding `refer.go`/`midcall.go` against a nil `endpointSide`; touching them risks
  regressions in the plain path. Mitigation: scope guards narrowly, cover with tests.

### Acceptance Criteria Coverage

| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Two-way Opus flows webphone ↔ plain-RTP | Yes | Requires the new write/encrypt seam + PBX dial in the secured branch + leg-oriented relay. |
| AC2 | Non-Opus (PCMU) passes through unchanged | Yes | Same path; relay moves payload bytes verbatim. Assumes matching PT both legs. |
| AC3 | SRTP decrypted/encrypted at the bridge | Yes | Decrypt = existing `ReadRTP`; encrypt = new leg write seam (pion local track). |
| AC4 | RTCP bridged alongside RTP | Partial | Needs an RTCP read/write seam on the rtcp-mux'd secured leg reconciled with the plain side's separate RTCP socket — not yet present on `WebRTCEndpoint`/`MediaLeg`. |
| AC5 | Never converts codecs | Yes | Guaranteed by passing payload through unchanged; no transcoding code added. |
| AC6 | Media-plane failure stays isolated | Yes | Mirror existing `copyUDP` log-and-stop behavior on the new secured read/write paths; teardown confined to the call. |
| NFR | No transcoding cost; SRTP↔SRTP later w/o forwarding-path change | Yes | Achieved by putting encrypt/decrypt in the leg (`MediaLeg` read+write) and keeping the relay security-agnostic. |
