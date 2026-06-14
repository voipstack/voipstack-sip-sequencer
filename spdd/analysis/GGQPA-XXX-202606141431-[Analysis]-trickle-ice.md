# SPDD Analysis: Trickle ICE (RFC 8838/8840) — STORY-001-020

## Original Business Requirement

> Source: `requirements/[User-story-20]trickle-ice.md` (story 4/5 of the
> SIP-over-WebSocket decomposition). Reproduced verbatim below.

### [STORY-001-020] Trickle ICE (RFC 8838)

#### Background
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

#### Business Value
- Provide prompt, reliable media setup for webphones that trickle candidates (the
  default browser behavior).
- Support both trickle and non-trickle clients without the webphone changing its
  configuration.

#### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-019]` (the ICE-lite media leg that validates
  connectivity checks).
- **Data assumptions:** The webphone delivers trickled candidates and an
  end-of-candidates indication after the initial offer, over the signaling path.
- **Integration points:** The browser's WebRTC stack (jssip / sip.js) over the
  network.
- **Business constraints:** ICE-lite only — the anchor still gathers host
  candidates only; trickle concerns the *remote* candidates the webphone sends.

#### Scope In
- Accept ICE candidates that arrive after the initial SDP offer and add them to the
  webphone leg's connectivity checks.
- Honor the end-of-candidates indication for the leg.
- Establish media once a valid candidate pair is found, whether candidates arrived
  in the offer or were trickled afterward.

#### Scope Out
- Bringing the ICE-lite leg up and DTLS-SRTP termination — `[STORY-001-019]`.
- The RTP bridge — `[STORY-001-021]`.
- The anchor gathering/trickling its own candidates beyond the single host
  candidate — out of scope (ICE-lite, host only).

#### Acceptance Criteria

##### AC1: A candidate trickled after the offer establishes media
**Given** a webphone that sends its SDP offer with no (or incomplete) candidates
and then trickles a host candidate
**When** the trickled candidate arrives
**Then** the anchor adds it to its connectivity checks and media establishes over
the resulting candidate pair.

##### AC2: End-of-candidates is honored
**Given** a webphone that has trickled its candidates and then signals
end-of-candidates
**When** the marker arrives
**Then** the anchor treats the remote candidate list as complete for that leg and
does not wait for further candidates.

##### AC3: A candidate arriving after the handshake is still handled
**Given** the media leg where a connectivity check is already in progress
**When** an additional candidate trickles in
**Then** it is incorporated without disrupting the leg that is coming up.

##### AC4: Both trickle and non-trickle offers work
**Given** one webphone that packs all candidates into the offer and another that
trickles them
**When** each places a call
**Then** media establishes in both cases without either webphone changing its
configuration.

##### Non-Functional Expectations
- Trickled candidates for one call must not affect ICE handling of any other call.

---

## Domain Concept Identification

### Existing Concepts (from codebase)
- **WebRTCEndpoint / pionEndpoint** (`internal/b2bua/webrtc.go`): the secured leg's
  media-library boundary. `pionEndpoint` already holds the `*webrtc.PeerConnection`
  and, after `Answer` runs `SetRemoteDescription`+`SetLocalDescription`, the PC is
  primed to accept *remote* ICE candidates. This is the exact object a trickled
  candidate must reach. The interface has no candidate-input method today.
- **SecuredLeg** (`webrtc.go`): the `MediaLeg` wrapping the endpoint; the per-call
  handle reachable from the `Call`. A trickle must traverse Call → SecuredLeg →
  WebRTCEndpoint.
- **MediaSession.endpointLeg** (`media.go`) and **Call.media** (`call.go`): how a
  call's secured endpoint leg is found from an in-dialog request.
- **handleInvite dialog matching** (`bridge.go`): the established pattern
  `dialogSrvCache.MatchDialogRequest(req)` → `calls.getByDialog(dss.ID)` for routing
  an in-dialog request to its `Call`. A trickle INFO must be matched the same way.
- **Engine SIP handlers** (`engine.go` `Run`): `OnInvite`/`OnAck`/`OnBye`/`OnRefer`/
  `OnRegister`/`OnNoRoute`. There is **no `OnInfo`**, so an `INFO` falls through to
  `OnNoRoute` → `proxyUnmanaged` and is forwarded to `cfg.NextHop` — a trickle INFO
  would currently be proxied to the PBX, not consumed.
- **proxyUnmanaged / forwardAndRelay** (`proxy.go`): the passthrough for unmanaged
  methods. Non-trickle INFO (e.g. DTMF, used by other endpoints) must keep flowing
  through this path untouched.
- **offerIsWebRTC** (`sdp.go`) and the plain SDP line-scanning helpers: the model for
  a new pure parser of a trickle SDP fragment.
- **Answer's `GatheringCompletePromise`** (`webrtc.go`): blocks until the anchor's
  *local* host-only gather completes (instant for ICE-lite). This concerns local
  candidates and is orthogonal to trickle, which is about *remote* candidates.

### New Concepts Required
- **Trickle candidate delivery (INFO interception)**: an engine handler that
  recognizes a webphone leg's in-dialog `INFO` carrying trickle ICE
  (`application/trickle-ice-sdpfrag`, RFC 8840), matches it to its `Call`, and feeds
  the candidates to that call's secured leg — while letting every other INFO proxy
  through unchanged.
- **Remote-candidate input on the leg**: a behavior on the secured leg / WebRTC
  boundary to add a remote ICE candidate and to signal end-of-candidates — defined in
  terms of ICE behavior, not the library's API (the boundary stays swappable).
- **Trickle SDP-fragment parsing**: a pure extractor of `a=candidate:` lines and the
  `a=end-of-candidates` marker from the INFO body.

### Key Business Rules
- **Trickle concerns remote candidates only**: the anchor stays ICE-lite, host-only;
  it ingests the webphone's candidates but gathers none beyond its single host
  candidate (governs that only candidate *input* is added; AC1, scope-out).
- **Both trickle and non-trickle must work unchanged**: candidates in the initial
  offer flow through `SetRemoteDescription`; trickled candidates flow through the new
  input path; neither requires webphone reconfiguration (governs AC4).
- **End-of-candidates is authoritative for the leg**: once signaled, the remote list
  is complete and the leg waits for no further candidates (governs AC2).
- **Per-call isolation**: a trickle for one call reaches only that call's leg; one
  call's candidates never affect another's ICE (governs NFE).
- **Non-trickle INFO is not hijacked**: only INFO with the trickle content type on a
  known secured webphone dialog is consumed; all other INFO proxies to the next hop
  (governs not regressing existing passthrough).

## Strategic Approach

### Solution Direction
Add an `INFO` interception at the signaling edge that routes trickle-ICE fragments to
the matching call's secured leg, and give the WebRTC boundary a remote-candidate input
seam:

`Webphone INFO (application/trickle-ice-sdpfrag) → engine OnInfo handler → match
dialog → Call → if endpoint leg is secured and body is a trickle fragment: parse
a=candidate / a=end-of-candidates → leg.AddCandidate(...) → pion AddICECandidate →
candidate joins connectivity checks → media establishes → respond 200 OK. Any other
INFO → existing proxyUnmanaged passthrough to NextHop.`

The 019 leg is otherwise unchanged: `Answer` already leaves the PeerConnection ready
to accept remote candidates after `SetRemoteDescription`, so trickle is purely
*additional input* over time. Non-trickle offers keep working because their candidates
are consumed at `SetRemoteDescription`; trickle offers (zero/partial candidates in the
offer) are completed by the new input path.

### Key Design Decisions
- **Where to consume trickle — new `OnInfo` handler vs. extend `OnNoRoute`**: a
  dedicated `OnInfo` handler keeps the trickle decision explicit and leaves
  `proxyUnmanaged` untouched for every other method. Recommendation: **register
  `OnInfo`**; inside it, only intercept INFO that (a) matches a known secured
  webphone dialog and (b) carries `application/trickle-ice-sdpfrag` — everything else
  delegates to `proxyUnmanaged`. This avoids regressing DTMF/other INFO passthrough.
- **Candidate-input surface — extend `WebRTCEndpoint` vs. a separate interface**:
  adding `AddCandidate` (and an end-of-candidates signal) to the existing
  `WebRTCEndpoint` keeps the secured leg cohesive and the pion type hidden.
  Recommendation: **extend `WebRTCEndpoint` + `SecuredLeg`** with a remote-candidate
  method; reach it from the `Call` via a type assertion on `endpointLeg` (only secured
  legs trickle). `MediaLeg` itself stays minimal (plain legs do not trickle).
- **End-of-candidates representation**: pion accepts an empty-candidate
  `AddICECandidate` as the end-of-candidates signal. Recommendation: model the
  boundary method so a parsed `a=end-of-candidates` maps to that signal, keeping the
  behavior library-agnostic at the interface.
- **SDP-fragment parsing — reuse pion vs. a pure local parser**: a small pure parser
  (mirroring `offerIsWebRTC`) extracting `a=candidate:` values and the
  `a=end-of-candidates` marker is testable without the network and matches existing
  SDP-handling style. Recommendation: **pure local parser**; pass each candidate
  value to the boundary.
- **Testing (AGENTS.md)**: drive a **real pion webphone** configured to trickle
  (offer with no candidates, then deliver candidates after) over the WS path; assert
  media reaches Connected via the trickled candidate, that end-of-candidates is
  honored, and that a non-trickle client still connects — no internal mocks.

### Alternatives Considered
- **Consume trickle via re-INVITE/UPDATE instead of INFO**: rejected — RFC 8840
  defines the SIP usage as `INFO` with `application/trickle-ice-sdpfrag`; re-INVITE on
  a secured leg is unrelated and currently nil-derefs (see risks).
- **Add `AddCandidate` to `MediaLeg`**: rejected — plain RTP legs have no candidates;
  it belongs on the secured leg / WebRTC boundary only.
- **Drop `GatheringCompletePromise` to "enable trickle"**: rejected — that promise is
  about the anchor's local host-only gather (instant), not remote trickle; removing it
  does not serve this story and risks an answer with no candidate line.
- **Anchor trickling its own candidates**: out of scope — ICE-lite, host-only.

## Risk & Gap Analysis

### Requirement Ambiguities
- **Delivery mechanism unstated**: the story says "over the signaling path" but not
  the exact SIP carrier. Assumed **`INFO` + `application/trickle-ice-sdpfrag`** (RFC
  8840), which is the standard SIP trickle usage and matches sip.js. Needs
  confirmation that the target webphones use this (not a proprietary header).
- **jssip trickle behavior**: jssip historically completes gathering before offering
  (non-trickle) in many configs, while sip.js trickles. "Both trickle by default" may
  be optimistic; AC4 (non-trickle must also work) covers the jssip case regardless.
- **Candidate `sdpMLineIndex`/`mid` in the fragment**: a single-audio-m-line leg
  implies index 0, but the fragment may carry an explicit `m=`/`a=mid`. Assume a
  single audio m-line; map to index 0 unless a mid is present.
- **Response semantics**: assumed a consumed trickle INFO is answered `200 OK`; a
  malformed fragment — `200 OK` (best-effort, ignore) vs. an error — needs a deliberate
  choice (lean to 200 + log, to not disrupt the coming-up leg, per AC3).

### Edge Cases
- **INFO arrives before the leg exists / after teardown**: a trickle for an unknown or
  ended dialog must be handled cleanly (no panic; respond 481/200 deliberately).
- **Non-trickle INFO (DTMF) on a webphone dialog**: must NOT be consumed as trickle —
  content-type gate, then proxy through.
- **Malformed or empty candidate fragment**: parse defensively; do not crash the leg.
- **Candidate after end-of-candidates**: late/extra candidate handling must not panic
  (pion may reject; wrap and log).
- **Trickle on a plain (non-secured) dialog**: there is no WebRTC leg; do not attempt
  candidate input — proxy or 200 deliberately.
- **handleReInvite on a secured call**: pre-existing latent bug —
  `handleReInvite` reads `call.media.endpointSide.localRTPPort`, which is **nil** for a
  secured webphone leg; a webphone re-INVITE would nil-deref. Out of scope for trickle,
  but should be guarded or explicitly deferred (likely STORY-001-021's bridge work).

### Technical Risks
- **INFO interception correctness**: must consume only trickle INFO on a matched
  secured dialog and proxy all else, or it regresses existing INFO passthrough.
  *Impact:* high. *Mitigation:* gate on dialog match + content type; delegate to
  `proxyUnmanaged` otherwise; behavioral test for a non-trickle INFO still proxying.
- **Reaching the leg from an in-dialog request**: `endpointLeg` is a `MediaLeg`; the
  candidate method lives on the secured impl. *Impact:* medium. *Mitigation:* type
  assert to the trickle-capable interface; if not secured, do not trickle.
- **Concurrency (AC3 / NFE)**: candidates arrive on the SIP handler goroutine while
  pion runs ICE on its own goroutines; `AddICECandidate` must be called race-free and
  isolated per call. *Impact:* medium. *Mitigation:* pion's `AddICECandidate` is
  concurrency-safe; per-call isolation is inherent (one endpoint per call);
  `go test -race`.
- **Candidate string format**: the value handed to pion must match
  `ICECandidateInit.Candidate` (the `candidate:...` attribute value, no `a=` prefix),
  with an `SDPMLineIndex`. *Impact:* medium. *Mitigation:* strip `a=candidate:`→
  `candidate:`; set index 0; unit-test the parser; integration-test the real handshake.
- **End-of-candidates mapping**: relies on pion's empty-candidate convention.
  *Impact:* low. *Mitigation:* cover with the real-client trickle test.

### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Trickled candidate establishes media | Yes | OnInfo → parse → `AddICECandidate`; real-client test asserts Connected via trickle |
| AC2 | End-of-candidates honored | Yes | map `a=end-of-candidates` → pion empty-candidate signal |
| AC3 | Candidate during handshake incorporated | Yes | pion `AddICECandidate` concurrency-safe; respond 200, never disrupt |
| AC4 | Both trickle and non-trickle work | Yes | offer candidates via `SetRemoteDescription` (019); trickled via new path |
| NFE | Per-call isolation | Yes (by design) | one endpoint per call; dialog-matched routing |

**Coverage summary:** all addressable. The weight is in the **INFO interception** (a
new `OnInfo` handler that consumes only trickle INFO on a matched secured dialog and
proxies everything else) and the **remote-candidate seam** on the WebRTC boundary
(`AddCandidate` + end-of-candidates, pion hidden). Items to settle before REASONS
Canvas: (1) **confirm the SIP carrier** is `INFO` + `application/trickle-ice-sdpfrag`
(RFC 8840); (2) **malformed-fragment response** policy (lean 200 + log); (3) note the
pre-existing **`handleReInvite` nil-deref on secured calls** as out-of-scope but
tracked. A real pion trickle client (offer-without-candidates → trickle → Connected)
de-risks AC1/AC2 before full design.
