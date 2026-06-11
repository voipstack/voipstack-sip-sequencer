# Story Decomposition: SIP over WebSocket support (story 3/5)

> Part of the WebSocket decomposition. Module overview and INVEST analysis in
> `[User-story-17]websocket-sip-signaling-transport.md`. Derived from
> `requirements/support-websocket-sip.md`.

---

## [STORY-001-019] WebRTC media leg — DTLS-SRTP termination + ICE-lite

### Background
Once a webphone can sign in and call over WebSocket (`[STORY-001-017]` /
`[STORY-001-018]`), its **media** still cannot reach the existing anchor. Browser
media is always WebRTC: **DTLS-SRTP + ICE + rtcp-mux**, with no plain-RTP option in
jssip / sip.js. The existing anchor is plain RTP. So the webphone-facing media leg
must be a real WebRTC endpoint: it answers **ICE-lite** (RFC 8445) — gathering only
host candidates on the publicly reachable anchor and validating the browser's
connectivity checks — and it terminates the **DTLS-SRTP** handshake to derive the
SRTP keys, honoring **rtcp-mux** (RTP and RTCP on one port). This story brings that
secured media leg *up*; forwarding its packets to the plain-RTP leg on the other
side is `[STORY-001-021]`.

Crucially, the leg's media security must be a **configurable property of the leg**,
not hardcoded to "webphone = SRTP". Today only DTLS-SRTP is required, but the design
must let an SRTP↔SRTP proxy (encrypted on both legs) be added later without
reworking the leg — so "is this leg secured, and how" is a per-leg setting.

Key points:
- Business value: a browser's WebRTC media can be terminated by the sequencer at
  all — the prerequisite for any audio to/from a webphone.
- The anchor matches how Asterisk, FreeSWITCH, and Janus run: a publicly reachable
  ICE-lite endpoint with host candidates only, so no TURN and no full ICE are
  needed.
- Per-leg security is configurable now to future-proof the SRTP↔SRTP proxy without
  rework.

### Business Value
- Provide webphones a media endpoint the sequencer can actually terminate (DTLS-SRTP
  + ICE), so browser audio can enter the system.
- Support a public-anchor deployment (ICE-lite, host candidates) with no TURN
  infrastructure, matching common SIP media servers.
- Enable a future SRTP↔SRTP proxy by making per-leg media security configurable
  rather than hardcoded.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-017]` / `[STORY-001-018]` (WebSocket signaling so a
  webphone can offer media), `[STORY-001-005]` (the existing RTP media anchor this
  leg will later bridge to).
- **Data assumptions:** The webphone's SDP offer carries DTLS fingerprint, ICE
  ufrag/pwd, host/srflx candidates, and rtcp-mux, as jssip / sip.js produce by
  default. The anchor has a configured, publicly reachable address to advertise as
  its host candidate.
- **Integration points:** The browser's WebRTC stack over the network; a Go WebRTC
  media library (candidate: pion/webrtc with `SettingEngine.SetLite(true)` +
  DTLS-SRTP, pure Go, no cgo) — an external library, not an external service.
- **Business constraints:** No transcoding anywhere. Server is the public anchor —
  ICE-lite only (host candidates), no TURN, no full ICE. Per-leg media security
  must be configurable.

### Scope In
- Answer ICE-lite on the webphone-facing leg: advertise a host candidate on the
  anchor's public address, and validate the browser's STUN connectivity checks
  (STUN handling is inherent to the ICE-lite implementation — no separate STUN
  deployment).
- Terminate the DTLS-SRTP handshake on that leg and derive the SRTP keys, honoring
  rtcp-mux.
- Represent the leg's media security as a configurable per-leg property (e.g.
  "DTLS-SRTP" vs "plain RTP"), so the opposite leg can independently be plain RTP
  today and SRTP later.

### Scope Out
- Forwarding/bridging media between this leg and the plain-RTP leg — `[STORY-001-021]`.
- Trickle ICE (candidates after the initial offer) — `[STORY-001-020]`.
- Full (non-lite) ICE, srflx/relay candidate gathering, and TURN — out of scope.
- SRTP↔SRTP on the opposite leg — out of scope now, but not precluded by design.
- Codec negotiation/conversion — codecs are negotiated end to end; never converted.

### Acceptance Criteria

#### AC1: The sequencer answers a webphone's WebRTC offer with ICE-lite
**Given** a webphone that sends an SDP offer with DTLS fingerprint, ICE
credentials, candidates, and rtcp-mux
**When** the sequencer answers
**Then** the answer is ICE-lite — it advertises a host candidate on the anchor's
configured public address and offers no TURN/relay candidate.

#### AC2: The DTLS-SRTP handshake completes and SRTP keys are derived
**Given** the ICE check between the webphone and the anchor has succeeded
**When** the DTLS handshake runs over the established path
**Then** it completes against the offered fingerprint and the anchor derives the
SRTP keys for the leg.

#### AC3: rtcp-mux is honored
**Given** the webphone offers rtcp-mux
**When** the media leg is established
**Then** RTP and RTCP for the webphone leg share a single port, as offered.

#### AC4: ICE connectivity checks are validated without a separate STUN server
**Given** the webphone sends STUN binding requests as part of its ICE checks
**When** they arrive at the anchor's host candidate
**Then** the anchor validates and responds to them using its own ICE-lite
implementation, with no externally deployed STUN server.

#### AC5: Per-leg media security is configurable, not hardcoded
**Given** a call where the webphone leg is DTLS-SRTP and the opposite leg is plain
RTP
**When** the legs are set up
**Then** each leg's security is determined by its own configured property — the
webphone leg secured, the opposite leg plain — and nothing assumes "webphone =
SRTP, other = RTP" as a fixed rule.

#### AC6: The host candidate uses the configured public address
**Given** the anchor is configured with a public address
**When** it gathers its ICE-lite candidate
**Then** the advertised host candidate carries that public address, so a webphone
on the open internet can reach it.

#### Non-Functional Expectations
- The media leg must be structured so that enabling SRTP on the opposite leg later
  (SRTP↔SRTP) requires no rework of this leg — only setting that leg's security
  property.
- The media library is a swappable boundary — the leg's behavior is defined by
  ICE-lite + DTLS-SRTP, not by a specific library's API leaking through.
