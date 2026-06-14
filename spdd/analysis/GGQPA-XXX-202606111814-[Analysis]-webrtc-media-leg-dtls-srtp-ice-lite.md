# SPDD Analysis: WebRTC media leg — DTLS-SRTP termination + ICE-lite (STORY-001-019)

## Original Business Requirement

> Source: `requirements/[User-story-19]webrtc-media-leg-dtls-srtp-ice-lite.md`
> (story 3/5 of the SIP-over-WebSocket decomposition). Reproduced verbatim below.

### [STORY-001-019] WebRTC media leg — DTLS-SRTP termination + ICE-lite

#### Background
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

#### Business Value
- Provide webphones a media endpoint the sequencer can actually terminate (DTLS-SRTP
  + ICE), so browser audio can enter the system.
- Support a public-anchor deployment (ICE-lite, host candidates) with no TURN
  infrastructure, matching common SIP media servers.
- Enable a future SRTP↔SRTP proxy by making per-leg media security configurable
  rather than hardcoded.

#### Dependencies and Assumptions
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

#### Scope In
- Answer ICE-lite on the webphone-facing leg: advertise a host candidate on the
  anchor's public address, and validate the browser's STUN connectivity checks
  (STUN handling is inherent to the ICE-lite implementation — no separate STUN
  deployment).
- Terminate the DTLS-SRTP handshake on that leg and derive the SRTP keys, honoring
  rtcp-mux.
- Represent the leg's media security as a configurable per-leg property (e.g.
  "DTLS-SRTP" vs "plain RTP"), so the opposite leg can independently be plain RTP
  today and SRTP later.

#### Scope Out
- Forwarding/bridging media between this leg and the plain-RTP leg — `[STORY-001-021]`.
- Trickle ICE (candidates after the initial offer) — `[STORY-001-020]`.
- Full (non-lite) ICE, srflx/relay candidate gathering, and TURN — out of scope.
- SRTP↔SRTP on the opposite leg — out of scope now, but not precluded by design.
- Codec negotiation/conversion — codecs are negotiated end to end; never converted.

#### Acceptance Criteria
- **AC1:** The sequencer answers a webphone's WebRTC offer with ICE-lite (host
  candidate on the configured public address, no TURN/relay).
- **AC2:** The DTLS-SRTP handshake completes against the offered fingerprint and the
  anchor derives the SRTP keys.
- **AC3:** rtcp-mux is honored (RTP + RTCP share one port).
- **AC4:** ICE connectivity checks (STUN binding) are validated by the anchor's own
  ICE-lite implementation, with no externally deployed STUN server.
- **AC5:** Per-leg media security is configurable, not hardcoded ("webphone = SRTP,
  other = RTP" is not a fixed rule).
- **AC6:** The advertised host candidate carries the configured public address.
- **Non-Functional:** Enabling SRTP on the opposite leg later (SRTP↔SRTP) must need
  no rework of this leg — only its security property; the media library is a
  swappable boundary (the leg is defined by ICE-lite + DTLS-SRTP, not a library API).

---

## Domain Concept Identification

### Existing Concepts (from codebase)
- **MediaSession** (`internal/b2bua/media.go`): relays RTP/RTCP between two
  `AnchorSide`s (endpoint-facing + PBX-facing), fanning out to taps. Today both sides
  are plain UDP. It is the relay that STORY-021 will use to bridge a secured leg to a
  plain leg; this story does not relay.
- **AnchorSide** (`media.go`): one anchored endpoint — a bound RTP socket and an odd
  RTCP socket on `mediaHost`, with atomically-swappable remote addresses. The plain
  leg today; the new secured leg is a *sibling* kind, not a modification of this one.
- **PortAllocator / PortPair** (`media.go`): even-RTP / odd-RTCP UDP port pairs. The
  secured (rtcp-mux) leg needs **one** port, not a pair — a different allocation
  shape.
- **Bridge media setup** (`bridge.go`, the `dialPBX`/anchor path ~378–458): builds
  `MediaSession{endpointSide: newAnchorSide(...), pbxSide: newAnchorSide(...)}` and
  rewrites SDP to the anchor via `rewriteToAnchor`. The `endpointSide` is exactly the
  seam where a webphone call must instead get a **secured WebRTC leg**.
- **SDP helpers** (`sdp.go`): `extractAudioCodecs`, `parseMedia`, `rewriteToAnchor`,
  `buildTapOffer` — all plain-RTP/SDP. The WebRTC answer (ice-lite, fingerprint,
  setup, candidate, rtcp-mux, ufrag/pwd) is new SDP territory.
- **`mediaHost`** (`engine.go`, from `cfg.SIP.Listen` host): the bind/advertise host
  for plain anchoring. The ICE-lite **public** candidate address (AC6) is a distinct,
  new concept — `mediaHost` is a bind address, not necessarily the public one.
- **Engine lifecycle / teardown** (`call.go`, `MediaSession.Close`): goroutine and
  socket ownership; a pion endpoint's lifecycle must hook into the same teardown.

### New Concepts Required
- **Media leg (per-leg security abstraction)**: a small interface over "an anchored
  media endpoint" with two implementations — **plain RTP** (today's `AnchorSide`) and
  **DTLS-SRTP/WebRTC** (new). The leg exposes a security property and a decrypted-RTP
  read/write seam that STORY-021's relay will use. This is the core of AC5/NFE.
- **Secured WebRTC leg**: the pion-backed endpoint that answers ICE-lite, gathers a
  host candidate on the public address, terminates DTLS-SRTP, derives SRTP keys, and
  honors rtcp-mux — exposing plaintext RTP/RTCP to the rest of the system.
- **WebRTC SDP answer**: building the ICE-lite/DTLS-SRTP answer to the webphone offer
  (a=ice-lite, a=fingerprint, a=setup:passive/actpass, a=candidate host, a=rtcp-mux,
  ICE ufrag/pwd), with the offered codecs echoed unchanged (no transcoding).
- **Public media address config**: an operator-set publicly reachable address the
  anchor advertises as its ICE-lite host candidate (AC6) — additive, optional config.
- **Media-library boundary**: an internal interface isolating pion so the leg's
  behavior is defined by ICE-lite + DTLS-SRTP, not by pion types leaking across the
  codebase (NFE).

### Key Business Rules
- **The leg defines its own security**: whether a leg is plain RTP or DTLS-SRTP is a
  per-leg property; nothing assumes the webphone side is the only secured side
  (governs the abstraction and AC5/NFE).
- **ICE-lite, host-only, public**: the anchor only gathers host candidates on the
  configured public address; it validates but never initiates connectivity checks; no
  TURN, no srflx, no full ICE (governs the pion `SetLite(true)` configuration and
  AC1/AC4/AC6).
- **rtcp-mux on the secured leg**: RTP and RTCP share one port on the webphone side
  (governs the single-port allocation vs. the plain leg's pair; AC3).
- **No transcoding**: the answer echoes the offered codecs unchanged; the leg
  encrypts/decrypts only (governs SDP answer; bridging is 021).
- **Library is swappable**: pion is hidden behind the media-leg interface (NFE).

## Strategic Approach

### Solution Direction
Introduce a **media-leg abstraction** at the endpoint seam and a **pion-backed
secured leg** behind it, bringing the webphone leg up without yet bridging packets:

`Webphone INVITE (WebRTC offer) → engine selects a secured leg for the endpoint side
(per-leg security = DTLS-SRTP) → pion SettingEngine.SetLite(true), host candidate =
configured public address → build SDP answer (ice-lite, fingerprint, rtcp-mux,
echoed codecs) → return in the SIP answer → browser runs ICE checks (validated by
pion) → DTLS-SRTP handshake completes → SRTP keys derived → leg is "up", exposing a
plaintext RTP/RTCP seam for STORY-021 to relay.`

The plain-RTP path is untouched: `AnchorSide` keeps satisfying the leg interface, and
the existing `MediaSession`/relay/tap code keeps working. Only the endpoint side of a
*webphone* call swaps to the secured leg. Bridging the two legs and end-to-end codec
flow remain STORY-021.

### Key Design Decisions
- **Media library — pion/webrtc (named) vs. low-level pion stack**: pion/webrtc with
  `SettingEngine.SetLite(true)` is the requirement's candidate and the least code; the
  low-level `pion/ice`+`pion/dtls`+`pion/srtp` stack gives finer control but much more
  glue. Recommendation: **pion/webrtc PeerConnection + SetLite**, accessed only
  through the media-leg interface so it stays swappable (NFE). Pure Go, no cgo —
  matches the build. (New direct dependency — a real choice to confirm.)
- **Per-leg security — interface vs. flag on `AnchorSide`**: a flag on `AnchorSide`
  would entangle plain and secured behavior; an interface keeps each leg cohesive.
  Recommendation: a **small leg interface** (plain and secured implementations); the
  relay (021) treats both uniformly via plaintext RTP/RTCP. This is what makes
  SRTP↔SRTP a later config flip, not a rework (AC5/NFE).
- **Public address — new config vs. reuse `mediaHost`**: `mediaHost` is a bind
  address; the ICE host candidate must be the *publicly reachable* address.
  Recommendation: add an **optional public-media-address config**; when unset, fall
  back to `mediaHost` (dev/local). Drives AC6.
- **rtcp-mux port shape**: the secured leg uses one port (pion/rtcp-mux); the plain
  leg keeps even/odd pairs. Recommendation: let the secured leg own its own
  port/socket via pion rather than forcing it through `PortAllocator`'s pair model —
  keep `PortAllocator` for plain legs.
- **Scope seam to STORY-021**: this story finishes at "keys derived, ICE validated,
  leg up" and exposes a plaintext RTP read/write handle; it does **not** wire that to
  `pbxSide`. Recommendation: define the seam (the leg interface's RTP I/O) now so 021
  is pure relay wiring.
- **Testing (AGENTS.md)**: drive with a **real pion-based webphone client** that
  offers WebRTC, runs ICE, and completes DTLS-SRTP against the anchor — a real fake,
  no mocks of internal code. Assert: ICE-lite answer shape, host candidate = public
  address, handshake completes, keys derived, rtcp-mux honored.

### Alternatives Considered
- **Terminate WebRTC with a cgo library (libwebrtc/GStreamer)**: rejected — the
  project is pure Go, no cgo (`go.mod`); pion is pure Go and explicitly named.
- **Transcode/bridge at the gateway instead of terminating WebRTC**: rejected —
  contradicts "no transcoding"; the leg encrypts/decrypts only.
- **Hardcode "webphone leg = SRTP"**: rejected — violates AC5/NFE; per-leg security
  must be a property so SRTP↔SRTP is a later flip.
- **Full ICE / TURN**: out of scope — the anchor is a public ICE-lite endpoint.

## Risk & Gap Analysis

### Requirement Ambiguities
- **Which call direction**: AC text is direction-agnostic ("a webphone's offer"). A
  webphone may be the *caller* (offer inbound) or the *callee* (offer outbound to it).
  Story 18 routes inbound calls to the webphone; does story 19 cover both offer
  directions or just webphone-as-caller? Needs scoping.
- **DTLS setup role**: browser offers `a=setup:actpass`; the anchor must choose
  active/passive. ICE-lite anchors are typically DTLS server (passive) — assumed, but
  unstated.
- **Public address form**: single IP? host:port? multiple (multi-homed)? AC6 says
  "the configured public address" (singular) — assume one IPv4 host.
- **Codec set in the answer**: "echo offered codecs unchanged" — but the answer's
  codecs should ultimately match the opposite leg (end-to-end). Story 19 answers
  before the opposite leg negotiates; reconciliation is 021/inherent — assume echo
  for now.

### Edge Cases
- **ICE check fails / DTLS handshake fails / times out**: the leg must fail cleanly
  and tear down (release port, stop pion goroutines) without affecting signaling or
  other calls.
- **Offer without rtcp-mux**: jssip/sip.js always send it, but a non-muxed offer
  should be rejected or handled deliberately, not crash.
- **Call torn down mid-handshake**: pion lifecycle must unwind on call teardown
  (integrate with `MediaSession.Close`).
- **Public address misconfigured (private IP advertised)**: the browser can't reach
  it; surface as a config/log concern, not a silent black-hole.
- **Many concurrent webphone legs**: each pion PeerConnection consumes a port and
  goroutines; interacts with `PortAllocator` exhaustion and resource limits.

### Technical Risks
- **pion-as-terminator integration**: using pion/webrtc to terminate ICE-lite +
  DTLS-SRTP and expose **plaintext** RTP/RTCP (for 021 to relay) requires the right
  pion API surface (PeerConnection + TrackLocal/TrackRemote or interceptor/RTP
  reader). *Impact:* high — the central new capability. *Mitigation:* prove a
  minimal pion SetLite handshake + plaintext-RTP read in a spike test first; keep it
  behind the leg interface.
- **SDP offer/answer interop with the B2BUA flow**: the existing flow rewrites plain
  SDP to the anchor; a WebRTC answer is structurally different and must slot into the
  same INVITE/answer path without breaking plain calls. *Impact:* high. *Mitigation:*
  branch on offer type (WebRTC vs plain) at the endpoint seam; leave plain untouched.
- **Per-leg abstraction refactor**: introducing the leg interface must not regress
  the existing plain-RTP relay/tap behavior or its tests. *Impact:* medium.
  *Mitigation:* `AnchorSide` adopts the interface with zero behavior change; cover
  with existing tests.
- **New dependency surface (pion/webrtc)**: pulls a sizable pure-Go tree; version
  pinning and `go.mod` growth. *Impact:* low–medium. *Mitigation:* pin a release;
  isolate behind the boundary.
- **ICE-lite correctness (AC4)**: the anchor must validate STUN connectivity checks
  itself; `SetLite(true)` provides this but must be configured to gather host only and
  not initiate checks. *Impact:* medium. *Mitigation:* behavioral test with a real
  pion client.
- **Concurrency/lifecycle**: pion runs its own goroutines; ownership and teardown must
  be race-free and integrated (`go test -race`). *Impact:* medium.

### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | ICE-lite answer, host candidate on public address, no TURN | Yes | pion `SetLite(true)` + host-only gather; build the WebRTC SDP answer |
| AC2 | DTLS-SRTP handshake completes, keys derived | Yes (core risk) | pion terminates DTLS-SRTP; verify against offered fingerprint |
| AC3 | rtcp-mux honored (one port) | Yes | pion rtcp-mux default; single-port leg (not the even/odd pair) |
| AC4 | ICE checks validated, no separate STUN server | Yes | inherent to pion ICE-lite; behavioral test with a real client |
| AC5 | Per-leg security configurable, not hardcoded | Yes | media-leg interface with plain + secured impls; security is a leg property |
| AC6 | Host candidate uses the configured public address | Yes | new optional public-media-address config; falls back to `mediaHost` |
| NFE | SRTP↔SRTP later needs no rework; library swappable | Yes (by design) | leg interface + pion hidden behind it; symmetric security property |

**Coverage summary:** all 7 addressable by design; the real weight is in **AC2** (pion
DTLS-SRTP termination exposing plaintext RTP) and the **per-leg media-leg
abstraction** (AC5/NFE) that keeps the plain path intact and makes STORY-021 a relay
exercise. Items to settle before REASONS Canvas: (1) **confirm pion/webrtc** as the
dependency (new direct dep, pure Go); (2) **offer-direction scope** — webphone as
caller only, or also callee; (3) **public-media-address config** name/shape (with
`mediaHost` fallback). A pion spike proving SetLite handshake + plaintext-RTP read de-
risks AC2 before full design.
