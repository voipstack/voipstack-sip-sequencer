# SPDD Analysis: Media fork to applications (stereo tap)

> Phase 0 (analysis) for `[STORY-001-010]`. Strategic level. The "How" (exact SDP synthesis,
> relay fan-out) is left to `/spdd-reasons-canvas`. Builds directly on the story-005 anchor.

## Codebase grounding (working notes)
- **Stories 001–005 implemented.** Media plane exists in `internal/b2bua`:
  - `media.go`: `PortAllocator` (even/odd pairs, `Acquire`/`Release`, `ErrPortsExhausted`);
    `AnchorSide` (RTP+RTCP `*net.UDPConn`, `remoteRTP/RTCP`, `pair`); `MediaSession`
    (`endpointSide`, `pbxSide`) with `relay(ctx)` = **4 fixed goroutines** (`copyUDP`) +
    `Close()` (once). `copyUDP(conn, dst)` reads `conn`, writes to `dst` via the **same**
    conn.
  - `sdp.go`: pure `parsePortRange`, `parseMedia(sdp)→(host,rtpPort)`,
    `rewriteToAnchor(sdp,host,rtpPort)` (rewrites first `c=`/`m=audio`, passes rest verbatim).
  - `bridge.go`: app-chain loop still uses the **serial app-SDP scaffolding** (D4 deferred
    in 005); the **call** anchor is built from the inbound offer (→PBX) + PBX answer
    (→endpoint); `call.media` started after PBX 2xx.
  - `call.go`: `Call.media *MediaSession`; `teardown` closes media + releases ports.
- **`config.Application`** = `{Name, URI, OnFailure}` — **no `media` field yet** (PRD §4
  adds it; this story introduces it). `config` validates + defaults `OnFailure`.
- **Per-app `media` (`tap`|`none`, default `none`)** and the **fork** are this story (PRD
  §4/§5). This is where the deferred D4 from story 005 (app-leg media) is resolved.
- `AGENTS.md`: pure SDP synthesis/port math; edge sockets/goroutines; real UDP fakes;
  `-race`; clear goroutine ownership.

## Original Business Requirement

> Complete `[STORY-001-010]` text — see `requirements/[User-story-10]media-fork-to-applications.md`.
> Summary: add per-app `media: tap|none` (default none). `tap` apps receive a **fork** of
> the call's audio as a **recvonly two-`m=audio` (stereo) session** — caller audio → stream
> 1, callee audio → stream 2 — **byte-for-byte, no mixing/transcoding**. `none` apps are
> offered audio `inactive` and receive no RTP. Fork ports from `rtp.port_range`, released on
> teardown. ACs: both directions reach the tap app (AC1), byte-for-byte separate streams
> (AC2), tap is recvonly / call unaffected (AC3), `none` apps get no media (AC4), default
> none (AC5), fork ports released (AC6).

## Domain Concept Identification

#### Existing Concepts (from codebase)
- **`config.Application`**: gains a `Media` field (`tap`|`none`, default `none`) — extends
  story 001's config (backward-compatible).
- **`MediaSession` / `copyUDP` relay**: must become **fan-out** capable — each call
  direction duplicated to zero-or-more tap sinks, in addition to the primary other side.
- **`PortAllocator`**: reused — a tap app needs **two** RTP/RTCP pairs (one per stream).
- **`rewriteToAnchor` / `parseMedia`**: reused; a **new** pure builder synthesizes the
  recvonly two-`m=audio` tap offer (more than a rewrite).
- **`bridge` app loop**: app-leg SDP handling is **reworked here** — `none` ⇒ offer
  inactive; `tap` ⇒ offer the dual-`m=` fork; this finally removes the serial scaffolding.
- **`Call` / `teardown`**: tracks the per-app tap sides; releases their ports + closes
  sockets on teardown.

#### New Concepts Required
- **Tap (per-app fork):** the sequencer's two recvonly anchored streams toward one tap app
  (stream 1 = caller direction, stream 2 = callee direction) + the app's learned remote RTP
  addresses.
- **Stereo tap offer (pure synthesis):** build a recvonly SDP with **two `m=audio`** lines,
  each pointing at a sequencer fork port, carrying the call's codec(s).
- **Relay fan-out:** the call's two directions, already relayed endpoint↔PBX, are each
  **copied** to the tap sink(s) for that direction.
- **Inactive media offer (`media: none`):** an `a=inactive` (or port-0) audio offer so the
  app negotiates no RTP.

#### Key Business Rules
- **tap receives both directions as separate streams:** caller→stream 1, callee→stream 2,
  over one app leg. AC1/AC2.
- **Copy only — no mix, no transcode:** fork is byte-for-byte duplication; codecs unchanged;
  streams kept separate (not mixed). AC2 / PRD §5.
- **tap is recvonly / call is sovereign:** the app only receives; anything it sends is not
  injected into the call; a tap never alters or (when failing) disrupts the call's media.
  AC3 + NFR.
- **none ⇒ no media:** offered `inactive`; no RTP, no fork ports. AC4.
- **default none:** omitted `media` ⇒ `none`. AC5.
- **fork ports from range, released on teardown:** AC6; no leak.
- **signaling unchanged:** every app still in the chain in order (003/004); `media` only
  controls audio. (PRD §4 orthogonality.)

## Strategic Approach

#### Solution Direction
- **Config:** add `Media MediaMode` to `config.Application` (`MediaTap`/`MediaNone`),
  default `none` at load, enum-validated (mirror the `OnFailure` pattern). Backward-compatible.
- **Pure SDP (sdp.go):** add `buildTapOffer(callOffer []byte, host string, p1, p2 int)
  ([]byte, error)` — synthesize a recvonly session with two `m=audio` lines (ports p1, p2),
  reusing the call offer's codec/rtpmap lines, `a=recvonly`. Add `buildInactiveOffer` (or a
  flag) for `none`. Add `parseTapAnswer` to read the app's two remote RTP addrs/ports.
- **Relay fan-out (media.go):** extend `MediaSession`/`copyUDP` so each direction can write
  to **multiple** destinations: the primary far side **plus** registered tap sinks. A tap
  sink = an app's remote RTP addr for that stream, fed from a dedicated seq fork socket.
  Restructure `copyUDP` → read once, write to N destinations (reuse buffer). Keep RTCP
  handling consistent (or fork RTP only — decide).
- **Bridge rework (app legs):** during the chain loop, per app:
  - `none` ⇒ offer `a=inactive`; no fork ports; app answers; no media wired.
  - `tap` ⇒ `Acquire()` two pairs; bind two fork `AnchorSide`s; offer `buildTapOffer`;
    on the app's 2xx, `parseTapAnswer` → set the tap sinks' remote addrs; register the tap
    with `call.media` (to be activated when the call relay starts).
  - Either way app-leg SDP is no longer the serial scaffolding (D4 resolved).
- **Wire taps when the anchor relay starts** (after PBX 2xx). Taps registered earlier (apps
  are INVITEd before the PBX leg) but packets only flow once the call media is up.
- **Teardown:** close tap sockets + release their port pairs (extend `call.media`/teardown).
- **Tests:** real UDP — a fake tap app that receives on its two negotiated ports; assert it
  gets caller audio on stream 1 and callee audio on stream 2, byte-for-byte; a `none` app
  gets nothing; default-none; call audio unaffected when a tap app is absent/failing; fork
  ports released.

#### Key Design Decisions (load-bearing — confirm before canvas)
- **D1 — Stereo = two `m=audio` lines (recvonly), not a mixed stereo stream.** (PRD-decided.)
  Caller→m-line 1, callee→m-line 2. → Confirm.
- **D2 — Fork ports: two RTP/RTCP pairs per tap app** (one per stream), from the existing
  allocator. → Rec yes. (Alternative: one pair with two SSRCs — rejected, less standard,
  needs RTP parsing we avoid.)
- **D3 — `media: none` offer shape: `a=inactive` vs port-0 decline.** → **Rec `a=inactive`**
  (keeps a normal m-line; broadest app compatibility; no RTP flows). Flag: some apps prefer
  port 0; revisit if needed.
- **D4 — Relay fan-out: extend `MediaSession` to N-destination writes per direction.**
  → Rec a small fan-out: each direction has a primary dst + a slice of tap dsts; `copyUDP`
  writes to all. Keep RRCP simple. → Confirm restructure of the 4-goroutine relay.
- **D5 — Fork RTP only, or RTP+RTCP to taps?** → **Rec RTP only to taps** (apps consume
  audio; RTCP to a recvonly observer is low value) — simpler, fewer ports. Flag as a small
  deviation; revisit if an app needs RTCP. (Call's own RTCP still relayed by 005.)
- **D6 — Multiple tap apps** → fan-out supports N taps per direction. → Rec yes (each tap
  independent; one failing/absent tap never affects others or the call).
- **D7 — Codec on the tap offer** = copy the call offer's audio codec/rtpmap lines so the
  app accepts the same payload types the forked packets carry. → Rec yes.

#### Alternatives Considered
- **Mix two directions into one stereo RTP:** rejected — needs decode/resample/mix
  (transcoding); PRD forbids. Two m-lines keep it copy-only.
- **Put tap apps in the serial media path:** rejected — listen-only apps would break the
  chain; fork is the model (PRD §5).
- **pion for media synthesis/relay:** rejected — stays stdlib + hand-built SDP (consistent
  with 005).

## Risk & Gap Analysis

#### Requirement Ambiguities
- **Tap offer codec vs negotiated codec:** the call's codec is finalized only at PBX answer,
  but app legs are INVITEd earlier. The tap offer must carry codec(s) the forked packets
  will actually use. Lean: offer the **endpoint's offered** codec list (superset); the
  forked bytes use whatever the call negotiated — if the PBX picks a codec the app didn't
  accept, the fork payload type may not match. **Risk** — see below; mitigation: offer the
  same codec list as the endpoint and rely on the PBX picking from it (common case single
  codec). Confirm in canvas.
- **`a=inactive` vs port 0** for `none` (D3).
- **RTCP to taps** (D5) — confirm RTP-only is acceptable.
- **Two m-lines ordering / which is caller vs callee** — define stream 1 = caller
  (endpoint→PBX direction), stream 2 = callee (PBX→endpoint). Document so apps can rely on it.

#### Edge Cases
- **Tap app fails / unreachable** — `on_failure` (004) governs the signaling; the fork
  simply isn't wired; the **call media must be unaffected** (NFR). Fan-out must tolerate a
  missing tap sink (no write target) without dropping the primary relay.
- **Port exhaustion during tap allocation** — a tap needs 2 pairs; if exhausted, treat like
  an app failure under its `on_failure` (skip ⇒ no fork, call continues; abort ⇒ fail call).
  Define this.
- **App answers with fewer/more m-lines than offered** — handle gracefully (use what it
  accepted; if it declines a stream, don't send it).
- **Tap registered before call media exists** — packets only flow after relay starts; ensure
  no nil-deref if the call fails before establishing.
- **Many taps + 100 calls** — goroutine/port growth (2 extra sockets/goroutines per tap);
  validate scale.

#### Technical Risks
- **Relay restructure regression:** changing `MediaSession`/`copyUDP` risks the working
  005 call relay. Mitigation: keep the endpoint↔PBX path behavior identical; add fan-out as
  an additive write-list; keep 005 media tests green; `-race`.
- **Codec mismatch on fork (above):** could deliver payloads the app can't decode.
  Mitigation: copy endpoint codec list to the tap offer; document single-codec assumption;
  full transcoding out of scope.
- **copyUDP source-port quirk:** 005's `copyUDP` writes via the reading socket; taps should
  send from dedicated fork sockets to the app — ensure the fan-out uses the right source
  socket so the app's latching/symmetric expectations hold. Verify in canvas.
- **Config change ripples:** adding `Media` to `Application` touches config tests; keep
  default-none backward-compatible.

#### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Tap gets both directions (2 streams) | Yes | dual-m offer + fan-out. |
| AC2 | Byte-for-byte, separate, no mix | Yes | copy per stream; assert payload equality. |
| AC3 | Tap recvonly; call unaffected | Yes | seq sendonly to app; fan-out additive. |
| AC4 | none ⇒ no media | Yes | `a=inactive`; no fork. |
| AC5 | default none | Yes | config default; mirror OnFailure. |
| AC6 | fork ports released | Yes | teardown releases tap pairs; soak. |
| NFR | tap failure doesn't disrupt call | Partial | fan-out tolerates missing sink; verify. |

**Net:** sizable — config `media` field + pure stereo-offer synthesis + relay fan-out +
bridge app-leg media rework (resolving 005's deferred D4) + per-app tap teardown.
Load-bearing decisions D1–D7 (two m-lines; two pairs/tap; `a=inactive` for none; fan-out
relay; RTP-only taps; N taps; copy endpoint codecs) to confirm before canvas. Watch the
**codec-match** risk and the **call-unaffected-on-tap-failure** invariant.
