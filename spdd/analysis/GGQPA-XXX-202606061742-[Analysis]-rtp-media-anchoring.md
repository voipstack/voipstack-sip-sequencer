# SPDD Analysis: RTP media anchoring & relay (the call)

> Phase 0 (analysis) for `[STORY-001-005]`. Strategic level. The "How" (exact SDP rewrite,
> socket loops) is left to `/spdd-reasons-canvas`. **This is the largest story** — it adds
> the media plane, which does not exist yet.

## Codebase grounding (working notes)
- **Stories 001–004 implemented.** `internal/b2bua` does **signaling only**:
  - `bridge.go` loops `cfg.Sequence`: each app is offered the **previous hop's SDP**
    (`offer` starts = inbound offer; after each app answers, `offer = appAnswerSDP`); the
    PBX is offered the **last app's answer**; the endpoint is answered with the **PBX
    answer**. This is the **serial SDP relay scaffolding** flagged in story 002 — purely
    opaque, media non-functional.
  - `call.go`: `Call{ inbound, appLegs []*OutboundLeg, pbxLeg }`; legs hold only `answerSDP`
    bytes; `teardown` BYEs live legs. No RTP/media anywhere.
  - `engine.go`: UA/server/client, UDP, dialog caches, `legTimeout`, `metrics` sink.
  - `config.RTP.PortRange` is still a **raw string** (`"10000-20000"`), parsed nowhere.
- **No media stack.** sipgo handles SIP only; there is **no RTP code, no SDP parser, no UDP
  media sockets**. All of that is new in this story.
- **Media model (PRD §5, decided earlier):** the **call** is the anchored
  `endpoint ↔ sequencer ↔ PBX` audio path; the sequencer rewrites SDP so all RTP flows to
  it and **copies packets** (no transcoding/mixing). **Per-app fork (`media: tap`) is
  `[STORY-001-010]`** — NOT this story. This story carries the call only.
- `AGENTS.md`: functional core (SDP parse/rewrite, port math, address mapping = pure) /
  edges (UDP sockets, goroutines); real in-memory fakes; clear goroutine ownership; `-race`.

## Original Business Requirement

> Complete `[STORY-001-005]` text — see `requirements/[User-story-5]rtp-media-anchoring.md`.
> Summary: anchor RTP for the **call** (endpoint↔seq↔PBX): own the media ports, rewrite
> `c=`/`m=` so both parties send to the sequencer, relay the two RTP directions byte-for-byte
> (no transcoding/mixing), draw ports from `rtp.port_range`, release on teardown, fail
> cleanly on exhaustion. **Forking to app legs is out of scope (story 010).** ACs: media
> flows through seq (AC1), unchanged payload (AC2), ports in range (AC3), released on
> teardown (AC4), bidirectional (AC5), exhaustion fails cleanly (AC6).

(See the story file for the full verbatim ACs; reproduced conceptually above to keep this
analysis focused — the canvas will restate them.)

## Domain Concept Identification

#### Existing Concepts (from codebase)
- **`bridge` SDP flow** (`bridge.go`): currently serial opaque relay. **Must be reworked** —
  the PBX can no longer be offered the app's answer; it must be offered the **sequencer's
  anchored** offer derived from the endpoint. This is the central change.
- **`config.RTP.PortRange`** (string): now actually **parsed** into a numeric range and
  consumed by a port allocator.
- **`Call` / `OutboundLeg` / `InboundDialog`**: gain an associated **media session**
  (anchored RTP for the endpoint and PBX sides). `teardown` must also release media.
- **`Engine`**: gains a **port allocator** (owns the range) shared across calls.

#### New Concepts Required
- **RTP anchor / media session:** per call, the sequencer's two anchored endpoints (one
  facing the endpoint, one facing the PBX) and the relay between them. Owns allocated ports.
- **Port allocator:** owns `rtp.port_range`; hands out RTP/RTCP port pairs; reclaims on
  release; fails when exhausted. Single owner, narrow interface, mutex-guarded.
- **SDP rewrite (pure):** parse an offer/answer; replace connection address (`c=`) and media
  port (`m=`) with the sequencer's; preserve everything else (codecs, attrs) unchanged.
- **RTP relay (edge):** UDP sockets + goroutines copying packets between the endpoint-facing
  and PBX-facing sockets, both directions, until teardown.
- **App-leg media (interim):** with the call anchored and fork deferred, app legs are offered
  **`a=inactive` / no media** for now (the `media: none` behavior that `[STORY-001-010]`
  makes configurable). App answers no longer feed the chain's SDP.

#### Key Business Rules
- **All media flows through the sequencer:** rewrite `c=`/`m=` on the endpoint and PBX legs
  so neither sends RTP directly to the other. AC1. Governs SDP rewrite + relay.
- **Copy, never transcode:** relay RTP payloads byte-for-byte; no codec change/mix/resample.
  AC2/PRD §5. Governs relay.
- **Bidirectional:** relay both directions (caller and callee audio) of the one call
  session. AC5.
- **Ports from the range, released on teardown:** allocate from `rtp.port_range`; free on
  call end; no leak. AC3/AC4. Governs allocator + teardown.
- **Exhaustion fails cleanly:** when the range is full, a new call fails (definite caller
  failure) without disrupting established calls. AC6. Governs allocator + bridge.
- **Call only, no fork:** app legs are not in the media path this story. (Boundary with 010.)

## Strategic Approach

#### Solution Direction
- **New `internal/b2bua/media` (or `rtp`/`sdp`) package**, split functional-core vs edge:
  - **Pure:** an SDP rewrite function (`rewriteToAnchor(sdp, host, rtpPort) ([]byte, error)`)
    that swaps `c=`/`m=` to the sequencer and leaves codecs/attrs intact; plus the
    **port-range parse** (`"10000-20000"` → `{min,max}`) and allocator math.
  - **Edge:** a **relay** that opens UDP sockets for the allocated ports and pumps packets
    between the endpoint-facing and PBX-facing sockets (both directions, + RTCP), under a
    per-call `context`, with one clear owner goroutine set.
- **Port allocator on `Engine`:** parse `cfg.RTP.PortRange` in `New` (fail fast on bad
  range); a mutex-guarded allocator hands out/reclaims RTP(/RTCP) pairs.
- **Rework `bridge` media flow** (the load-bearing change):
  - Endpoint offer → allocate the endpoint-facing anchor; learn endpoint RTP addr from its
    SDP.
  - **App legs:** offer `inactive` media (no RTP); ignore their answers for media. They stay
    in the **signaling** chain (003/004) for ordering/gatekeeping.
  - PBX leg: allocate the PBX-facing anchor; offer the PBX the **sequencer's anchored offer**
    (seq address/port, endpoint's codecs); learn PBX RTP addr from its answer.
  - Answer the endpoint with the **sequencer's anchored answer** (seq address/port).
  - Start the relay between the two anchors; on teardown stop it and release ports.
- **Tests:** real UDP — fake endpoint + fake PBX that actually send RTP packets on the
  negotiated (rewritten) ports; assert packets arrive byte-for-byte via the sequencer, in
  both directions, on ports within range, and that ports free after teardown; an exhaustion
  test with a tiny range.

#### Key Design Decisions (load-bearing — confirm before canvas)
- **D1 — SDP handling: hand-parse vs a library (e.g. `pion/sdp`).**
  Trade-off: hand-parsing only the `c=`/`m=` lines is tiny, dependency-free, and matches
  "rewrite minimally"; `pion/sdp` is robust but a new dependency and more API surface.
  → **Rec: hand-parse** the few lines we touch (rewrite `c=`/`m=`, read remote addr/port),
  pass the rest through untouched. Pure + testable. Revisit if SDP variety bites.
- **D2 — RTP relay: hand-rolled `net.UDPConn` vs `pion` media stack.**
  Trade-off: we only **copy packets** (no jitter buffer, no decode) → raw UDP read/write
  loops are the simplest correct thing and honor "no processing"; pion is heavyweight for a
  pure relay. → **Rec: hand-rolled UDP relay** (two sockets per call, goroutines copying
  both ways). 
- **D3 — Remote address discovery: SDP-signaled vs latching.**
  Trade-off: use the address/port from the peer's SDP (simple, correct on open networks) vs
  **latch** (learn the real source from the first received packet — needed behind NAT).
  → **Rec: SDP-signaled for v1**, note NAT as a known limitation (symmetric latching a later
  enhancement). 
- **D4 — App-leg media while fork is deferred (010).**
  Trade-off: offer apps `a=inactive` (clean, == future `media: none`) vs leave the serial
  relay to apps (inconsistent with anchoring). → **Rec: offer app legs inactive media**;
  app answers no longer feed the chain SDP. Sets up 010 cleanly. (This is the behavior
  change most likely to ripple into 003/004 tests — verify they assert signaling, not SDP.)
- **D5 — RTCP.** → **Rec: allocate RTP/RTCP as an even/odd pair and relay both** (standard);
  cheap and avoids RTCP black-holing.

#### Alternatives Considered
- **pion/webrtc or full media server:** rejected — massive overkill for a byte-copy relay;
  pulls in ICE/DTLS/codecs we explicitly don't want (PRD: no transcoding, plain RTP).
- **Keep apps in the media path (serial relay):** rejected — contradicts the anchored
  call + fork model (PRD §5); listen-only apps would break the chain.
- **Defer SDP rewrite, only open sockets:** rejected — without rewrite, parties send RTP to
  each other, not the anchor (AC1 fails).

## Risk & Gap Analysis

#### Requirement Ambiguities
- **App-leg media interim (D4):** the story is "the call only," but anchoring forces a
  decision on what app legs are offered now (inactive vs nothing). Must be explicit; affects
  003/004 regression.
- **Codec passthrough scope:** "byte-for-byte" assumes both sides agree on a codec; the
  sequencer does not pick/negotiate codecs — it copies the endpoint's offer to the PBX. If
  the PBX answers a subset, the endpoint anchored answer must reflect the PBX's chosen codec.
  Confirm the rewrite carries the PBX's answer codec back to the endpoint.
- **Hold/inactive direction & mid-call:** re-INVITE/hold media is story 007; this story is
  initial negotiation only — but the relay must tolerate one-directional silence.
- **IPv4/IPv6 / multiple m-lines / non-audio m-lines:** scope is one audio stream; extra
  m-lines (video) — reject or pass inactive? Lean: handle the first audio m-line; note
  others as boundary.

#### Edge Cases
- **Port exhaustion** (AC6) → allocate fails → caller gets clean failure (e.g. 503/486),
  established calls untouched, no half-allocated leak.
- **Allocation succeeds but socket bind fails** (port race with another process) → retry
  next pair or fail cleanly; release the reserved pair.
- **Teardown mid-setup** (call fails before relay starts) → any reserved ports released.
- **One side never sends RTP** → relay idles; no busy-loop; exits on ctx cancel.
- **Asymmetric/garbage packets** → copied as-is (no parsing); size-bounded reads.
- **Bad `rtp.port_range`** (reversed, non-numeric, zero span) → **fail fast at `New`** (this
  is the deferred "value validation" from story 001, now owned here).

#### Technical Risks
- **Concurrency/leaks:** per-call media goroutines + a shared allocator ⇒ race/leak risk.
  Mitigation: allocator behind a mutex; relay goroutines owned by the call, exit on ctx
  cancel; release ports in teardown; `-race` + a soak test (many calls, assert ports return
  to free and goroutines exit).
- **Bridge rework regression:** changing the SDP flow risks breaking 002/003/004 signaling.
  Mitigation: keep signaling sequence identical; only change SDP bodies + add media; keep
  those tests green; add media tests separately.
- **SDP variety:** hand-parsing may miss formats. Mitigation: rewrite only `c=`/`m=`, pass
  the rest verbatim; test with the fakes' real SDP.
- **Performance (NFR 100 calls):** a relay is 2 sockets + 2 goroutines per call (×2 for
  RTCP) → ~hundreds of goroutines; fine for Go, but validate no per-packet allocation in the
  copy loop (reuse buffers).

#### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Media flows through the sequencer | Yes | SDP rewrite to anchor + relay. |
| AC2 | Payload unchanged (no transcode) | Yes | byte-copy relay; assert equal payloads. |
| AC3 | Ports within `rtp.port_range` | Yes | allocator bounded by parsed range. |
| AC4 | Ports released on teardown | Yes | release in teardown; soak test. |
| AC5 | Bidirectional audio | Yes | relay both directions. |
| AC6 | Exhaustion fails cleanly | Yes | allocate-fail → caller failure; others unaffected. |
| NFR | No audible latency/loss @100 calls | Partial | buffer reuse; sanity load, not a hard gate. |

**Net:** large but tractable — a new media package (pure SDP-rewrite/port-math + edge UDP
relay) + an `Engine` port allocator + a reworked `bridge` media flow, with app legs offered
inactive media until the fork (010). **Load-bearing decisions D1–D5** (hand-parse SDP;
hand-rolled UDP relay; SDP-signaled addressing; app legs inactive; relay RTCP) should be
confirmed before the canvas. Also: `rtp.port_range` value validation moves here (fail fast
at `New`). No blockers.
