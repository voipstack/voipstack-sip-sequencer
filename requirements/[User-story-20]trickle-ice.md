# Story Decomposition: SIP over WebSocket support (story 4/5)

> Part of the WebSocket decomposition. Module overview and INVEST analysis in
> `[User-story-17]websocket-sip-signaling-transport.md`. Derived from
> `requirements/support-websocket-sip.md`.

---

## [STORY-001-020] Trickle ICE (RFC 8838)

### Background
Browsers do not always have all their ICE candidates ready when they send the SDP
offer — they **trickle** them: send the offer immediately and deliver additional
candidates as they are discovered, ending with an end-of-candidates marker (RFC
8838). The ICE-lite media leg from `[STORY-001-019]` must accept these trickled
candidates and incorporate them into connectivity checks, and honor
end-of-candidates, so media establishes promptly with jssip and sip.js webphones,
which trickle by default. Without this, media setup waits on (or fails for) clients
that do not pack every candidate into the initial offer.

Key points:
- Business value: media comes up fast and reliably for real browser clients, which
  trickle candidates rather than blocking the offer until gathering completes.
- A focused addition to the established ICE-lite leg — same leg, candidates now
  arriving over time.
- Required for seamless jssip / sip.js compatibility (both trickle by default).

### Business Value
- Provide prompt, reliable media setup for webphones that trickle candidates (the
  default browser behavior).
- Support both trickle and non-trickle clients without the webphone changing its
  configuration.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-019]` (the ICE-lite media leg that validates
  connectivity checks).
- **Data assumptions:** The webphone delivers trickled candidates and an
  end-of-candidates indication after the initial offer, over the signaling path.
- **Integration points:** The browser's WebRTC stack (jssip / sip.js) over the
  network.
- **Business constraints:** ICE-lite only — the anchor still gathers host
  candidates only; trickle concerns the *remote* candidates the webphone sends.

### Scope In
- Accept ICE candidates that arrive after the initial SDP offer and add them to the
  webphone leg's connectivity checks.
- Honor the end-of-candidates indication for the leg.
- Establish media once a valid candidate pair is found, whether candidates arrived
  in the offer or were trickled afterward.

### Scope Out
- Bringing the ICE-lite leg up and DTLS-SRTP termination — `[STORY-001-019]`.
- The RTP bridge — `[STORY-001-021]`.
- The anchor gathering/trickling its own candidates beyond the single host
  candidate — out of scope (ICE-lite, host only).

### Acceptance Criteria

#### AC1: A candidate trickled after the offer establishes media
**Given** a webphone that sends its SDP offer with no (or incomplete) candidates
and then trickles a host candidate
**When** the trickled candidate arrives
**Then** the anchor adds it to its connectivity checks and media establishes over
the resulting candidate pair.

#### AC2: End-of-candidates is honored
**Given** a webphone that has trickled its candidates and then signals
end-of-candidates
**When** the marker arrives
**Then** the anchor treats the remote candidate list as complete for that leg and
does not wait for further candidates.

#### AC3: A candidate arriving after the handshake is still handled
**Given** the media leg where a connectivity check is already in progress
**When** an additional candidate trickles in
**Then** it is incorporated without disrupting the leg that is coming up.

#### AC4: Both trickle and non-trickle offers work
**Given** one webphone that packs all candidates into the offer and another that
trickles them
**When** each places a call
**Then** media establishes in both cases without either webphone changing its
configuration.

#### Non-Functional Expectations
- Trickled candidates for one call must not affect ICE handling of any other call.
