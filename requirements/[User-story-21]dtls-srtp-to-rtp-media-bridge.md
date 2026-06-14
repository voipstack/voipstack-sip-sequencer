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
