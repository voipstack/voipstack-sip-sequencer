# SPDD Analysis: WebSocket SIP signaling transport (STORY-001-017)

## Original Business Requirement

> Source: `requirements/[User-story-17]websocket-sip-signaling-transport.md`
> (story 1/5 of the SIP-over-WebSocket decomposition; full INVEST analysis and
> split strategy live in that file). Reproduced verbatim below.

### [STORY-001-017] WebSocket SIP signaling transport

#### Background
Web-based SIP clients (webphones built with jssip or sip.js) speak SIP over
WebSocket (RFC 7118); they cannot use the UDP/TCP/TLS listeners the sequencer
exposes today. This story adds WebSocket as another inbound transport, peer to the
existing ones: an unencrypted **WS** endpoint (development/testing only) and an
encrypted **WSS** endpoint (production). On connect, the sequencer negotiates the
`sip` WebSocket subprotocol (`Sec-WebSocket-Protocol: sip`); thereafter each
WebSocket text/binary frame carries a SIP message that is parsed and routed
**exactly as on the other transports** — the sequencer stays a proxy / B2BUA and
gains no WebSocket-specific signaling behavior. The flow is held open against NAT
and idle timeouts with WebSocket ping/pong. The WebSocket listeners run in
parallel with the existing listeners; a config with no WebSocket keys opens no
WebSocket listener at all.

Key points:
- Business value: browser webphones can reach the sequencer with no change to
  their application; SIP semantics are preserved end to end.
- First place WebSocket terminates on the wire — the foundation every other
  WebSocket story builds on.
- Fully backward compatible and opt-in — existing configs and clients are
  unaffected.

#### Business Value
- Provide jssip / sip.js webphones a standards-based way (RFC 7118) to reach the
  sequencer without changing their application.
- Support a migration path — WebSocket and the existing UDP/TCP/TLS listeners run
  simultaneously, so clients adopt WebSocket at their own pace.
- Preserve standard SIP semantics — WebSocket is just the transport; routing and
  B2BUA behavior are unchanged.

#### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-001]` (configuration loading — this adds the
  WebSocket listener config), existing inbound listening and B2BUA behavior from
  earlier module-001 stories. For WSS, the TLS material from the TLS module
  (`[User-story-13]`/`[User-story-14]`) is reused to secure the WebSocket
  connection.
- **Data assumptions:** The operator supplies an optional WebSocket listener
  configuration (a `ws` and/or `wss` listen address); WSS names a usable
  certificate.
- **Integration points:** Browser SIP clients (jssip, sip.js) over the network —
  the external peers initiating WebSocket connections.
- **Business constraints:** WebSocket and existing listeners run in parallel;
  enabling WebSocket must not change existing-listener behavior; omitting the
  WebSocket config opens no WebSocket listener. SIP over WebSocket must be parsed
  and routed identically to the other transports.

#### Scope In
- Open an inbound **WS** listener and an inbound **WSS** listener (each optional)
  on their configured addresses, alongside the existing UDP/TCP/TLS listeners
  (parallel, separate ports).
- Complete the WebSocket upgrade handshake and negotiate the `sip` subprotocol
  (`Sec-WebSocket-Protocol: sip`); refuse connections that do not offer `sip`.
- Extract a SIP message from each inbound WebSocket frame and hand it to the
  existing parsing/routing stack; frame outbound SIP messages back over the same
  WebSocket connection.
- Honor WebSocket ping/pong to keep an otherwise-idle flow open through NAT/idle
  timeouts.

#### Scope Out
- SIP Outbound registration flows, reg-id / instance-id, connection reuse, Path,
  and RFC 5626 CRLF keep-alive — `[STORY-001-018]`.
- All media-plane work (DTLS-SRTP, ICE-lite, trickle ICE, the RTP bridge) —
  `[STORY-001-019]` … `[STORY-001-021]`.
- Building the TLS material that secures WSS — reused from the TLS module.
- Certificate reload without restart (future consideration).

#### Acceptance Criteria
- **AC1:** A webphone connects over WS and its signaling is routed normally.
- **AC2:** A webphone connects over WSS in production.
- **AC3:** SIP over WebSocket is routed identically to other transports.
- **AC4:** A client that does not offer the `sip` subprotocol is rejected.
- **AC5:** WebSocket and existing listeners are active simultaneously.
- **AC6:** Omitting the WebSocket config opens no WebSocket listener.
- **AC7:** WebSocket ping/pong holds an idle flow open.
- **Non-Functional:** A slow/failed WebSocket upgrade from one client must not
  block accepts on the other listeners; a SIP message over WebSocket must be parsed
  by the same code path as the other transports (no WebSocket-specific semantics).

---

## Decision Log
- **Use sipgo's ws/wss transport as-is — no fork, no connection wrapper** (decided
  by product owner). Consequences, now reflected in the story ACs:
  - **AC4 rescoped** from "reject non-`sip` clients" → "negotiate the `sip`
    subprotocol". Stock sipgo advertises/negotiates `sip` (`transport_ws.go:20-22,
    112`) but does not reject omitters; jssip / sip.js always offer `sip`, so this
    is an accepted limitation, not a gap.
  - **AC7 removed** (WebSocket ping/pong). Stock sipgo does not handle ping/pong
    (`transport_ws.go:408-413` commented out). Idle-flow keep-alive is delegated to
    TCP keep-alive now and the RFC 5626 CRLF keep-alive in `STORY-001-018`.
  - **Binary frames out of scope** — sipgo handles `OpText` only; SIP over
    WebSocket from the named clients is text.
- Net effect: STORY-017 is a **pure wiring story** on the existing inbound-TLS
  pattern; no upstream/dependency work remains.

## Domain Concept Identification

### Existing Concepts (from codebase)
- **Engine** (`internal/b2bua/engine.go`): owns the sipgo `UserAgent`, `Server`,
  `Client`, dialog caches, and the listener lifecycle. Already runs the plain UDP
  listener and (when configured) the inbound TLS listener **in parallel on the same
  `srv`** via an `errgroup`, with fail-fast semantics — the model this story
  extends.
- **SIP transport selector** (`config.Transport`, `internal/config/config.go`):
  enum `udp` / `tcp` / `tls`. WebSocket adds new *inbound listeners*, not
  necessarily a new value of this outbound selector — relationship to clarify
  below.
- **Listener configuration** (`config.SIP{Listen}`, `config.TLS{Listen,
  TLSProfile, Resolved}`): the plain listener is a scalar address; the TLS listener
  is an optional block referencing a named profile. WS/WSS listeners mirror this
  shape.
- **TLS provider** (`internal/tlsprov`, `Provider.ServerConfig(ResolvedTLSProfile)
  → *tls.Config`): builds the server TLS context from a resolved profile. WSS
  reuses this exactly — it is the same TLS termination as the `tls.listen` listener,
  just with a WebSocket framing on top.
- **sipgo WebSocket transport** (dependency `github.com/emiago/sipgo` v1.4.0):
  `Server.ListenAndServe(ctx, "ws", addr)` and
  `Server.ListenAndServeTLS(ctx, "wss", addr, *tls.Config)` are **already
  supported** (`server.go:147`, `:196`); the transport advertises the `sip`
  subprotocol by default (`sip/transport_ws.go:20-22`,
  `WebSocketProtocols = ["sip"]`). SIP message framing over WebSocket lives inside
  sipgo, not this codebase.
- **Transport-agnostic B2BUA handlers** (`Engine.handleInvite`, `OnAck`, `OnBye`,
  `OnRefer`, `OnNoRoute` in `engine.go`/`bridge.go`/`proxy.go`): routing and dialog
  handling are bound to the shared `srv`, independent of which transport accepted
  the request. This is what makes AC3 (identical routing) hold by construction.
- **Audit listener wrapper** (`auditListener`/`auditConn`, `engine.go`): the
  established pattern for wrapping an accepted-connection listener to add
  cross-cutting behavior (there: sanitized handshake-failure logging) without
  touching sipgo internals. A candidate vehicle for the subprotocol/keep-alive gaps
  below.

### New Concepts Required
- **WebSocket listener config**: optional `ws` (plain) and `wss` (TLS) listener
  blocks in the YAML, additive and parallel to `sip.listen` and `tls.listen`. `wss`
  references a named `tls_profile` exactly as `tls.listen` does, so it resolves
  through the existing profile model with zero new TLS concepts.
- **WSS server context binding**: a per-WSS-listener `*tls.Config` built at startup
  from its resolved profile via `tlsprov`. The engine today builds a single
  `tlsServerConf` from `cfg.TLS.Listen` only; WSS needs its own resolved profile →
  its own server config (fail-fast at `New`, like the TLS listener).
- **Subprotocol-rejection guard** (conditional — see risks): a guard that refuses a
  WebSocket upgrade from a client that does not offer `Sec-WebSocket-Protocol: sip`,
  because stock sipgo does not enforce this (it only advertises `sip`).
- **WebSocket keep-alive handling** (conditional — see risks): ping/pong handling,
  because stock sipgo v1.4.0 has it commented out.

### Key Business Rules
- **Additive / opt-in**: a config with no `ws`/`wss` keys opens no WebSocket
  listener and behaves exactly as today — governs the listener-config and Engine
  startup concepts (mirror the existing `cfg.TLS.Listen != ""` gate).
- **Parallel, non-interfering listeners**: WS/WSS bind separate ports and run on the
  same `srv`; enabling them must not change UDP/TCP/TLS behavior — governs the
  Engine `Run` errgroup.
- **Transport transparency**: a SIP message arriving over WebSocket is parsed and
  routed by the same code path as every other transport — governs the B2BUA
  handlers (no WebSocket branch anywhere in routing).
- **`sip` subprotocol is mandatory on the wire**: the negotiated subprotocol must be
  `sip`; a client not offering it must be refused — governs the upgrade path.
- **Fail-fast on bad TLS material**: a WSS listener with an unloadable/invalid
  certificate must abort startup, not run degraded — governs WSS context build at
  `New` (mirror `tlsServerConf`).

## Strategic Approach

### Solution Direction
Extend the **existing inbound-TLS-listener pattern** rather than introduce a new
transport subsystem. Concretely, the data flow is:

`YAML ws/wss block → config resolve (reuse tls_profile model) → Engine.New builds
per-WSS *tls.Config via tlsprov (fail-fast) → Engine.Run adds g.Go goroutines
calling srv.ListenAndServe(ctx,"ws",addr) and srv.ListenAndServeTLS(ctx,"wss",addr,
conf) inside the same errgroup → sipgo terminates WebSocket + advertises sip
subprotocol → SIP messages flow into the unchanged OnInvite/OnBye/OnNoRoute
handlers.`

Because sipgo already implements ws/wss transport and the B2BUA handlers are
transport-agnostic, the **happy-path ACs (AC1, AC2, AC3, AC5, AC6) are largely a
wiring exercise** on a proven boundary. The two non-trivial ACs (AC4, AC7) are gaps
in stock sipgo and drive the real design decisions below.

### Key Design Decisions
- **Config shape — separate `ws`/`wss` blocks vs. a transport list**: trade-off is
  consistency vs. surface area. Recommendation: **mirror `sip.listen` /
  `tls.listen`** with optional `ws.listen` (scalar) and a `wss` block
  (`listen` + `tls_profile`). Rationale: reuses the resolved-profile model and the
  `Listen != ""` opt-in gate verbatim; smallest delta; AC6 falls out for free.
- **WSS TLS context — reuse `tlsprov.ServerConfig`**: trade-off is none material —
  WSS *is* TLS termination. Recommendation: build a dedicated `*tls.Config` per WSS
  listener at `New`, fail-fast, exactly like `tlsServerConf`. Rationale: no new TLS
  verification/cert concepts; one profile can serve `tls.listen` and `wss` alike.
- **AC4 subprotocol rejection — guard vs. accept sipgo's behavior**: stock sipgo
  *advertises* `sip` but does **not reject** a client that fails to offer it
  (`sip/transport_ws.go:116-145` — the `Upgrader` unconditionally returns the `sip`
  header and upgrades; there is no `Protocol`/negotiation guard). Trade-off:
  strict-compliance + AC4-as-written vs. minimal code. Recommendation: **add a thin
  upgrade guard** (in the spirit of `auditListener`) that inspects the client's
  `Sec-WebSocket-Protocol` request header and refuses the upgrade when `sip` is
  absent. Rationale: AC4 is explicit ("rejected"); browsers always send `sip`, so
  the guard is invisible in practice but makes the behavior conformant and testable.
- **AC7 keep-alive — where ping/pong lives**: stock sipgo v1.4.0 has ping/pong
  **commented out** (`sip/transport_ws.go:408-413`); inbound non-text frames
  (including Ping) are discarded and no Pong/server-ping is emitted. Trade-off: fork
  sipgo vs. wrap the connection vs. lean on lower-layer keep-alive. Recommendation
  (to confirm with maintainers): **wrap the accepted WS connection** to answer
  inbound Ping with Pong (and optionally emit periodic server pings) before sipgo
  reads, keeping sipgo unforked — or, if that proves brittle, rely on TCP keepalive
  plus the RFC 5626 CRLF keep-alive arriving in `STORY-001-018` and **renegotiate
  AC7's scope**. This is the highest-risk decision in the story.
- **Testing approach (per AGENTS.md)**: the webphone is an external client → test
  with a **real WebSocket/SIP client** (sipgo's own ws client or a `gobwas/ws`
  client) as a real fake, not a mock; model on existing `transport_test.go` /
  `tls_listener_test.go`. Do not mock internal engine code.

### Alternatives Considered
- **A standalone WebSocket gateway process / separate HTTP server in front of SIP**:
  rejected — the requirement is explicit that WebSocket is *just a transport*, not a
  gateway; sipgo already terminates ws/wss on the shared server, so a separate
  process adds a hop, breaks transport transparency, and duplicates routing.
- **Adding `ws`/`wss` as values of the outbound `config.Transport` enum now**:
  rejected for this story — STORY-017 is *inbound* only; outbound/flow concerns are
  STORY-018. Conflating them violates the split and YAGNI.
- **Forking sipgo to enable ping/pong + subprotocol rejection**: held as a fallback,
  not the default — a fork is a maintenance burden; a connection wrapper keeps the
  dependency stock if it proves sufficient.

## Risk & Gap Analysis

### Requirement Ambiguities
- **AC7 "honor WebSocket ping/pong"**: does this mean respond to client pings,
  proactively send server pings, or both? The interval/source is unspecified and
  interacts with STORY-018's CRLF keep-alive. Needs a concrete keep-alive policy.
- **"text/binary frame carries a SIP message"**: RFC 7118 permits binary, but
  jssip/sip.js use **text** frames, and stock sipgo only handles `OpText`
  (`sip/transport_ws.go:393` discards non-text). Is binary-frame support actually
  in scope, or is "text" sufficient? Recommend scoping to text (matches real
  clients) and noting binary as out of scope.
- **WS vs. WSS independence**: may both be enabled at once? Assumed yes (each
  optional, parallel), consistent with `sip.listen` + `tls.listen`, but not stated.
- **Contact/Via host for WebSocket**: which host/transport token the sequencer
  advertises in Contact for a ws-accepted dialog is unspecified; sipgo derives it,
  but it matters once STORY-018 routes back over the flow.

### Edge Cases
- **Client offers a subprotocol list including `sip` among others**: the guard must
  accept (sip present), not require sip be the sole/first entry.
- **Plain HTTP / non-WebSocket request to the ws port**: must be cleanly refused
  without affecting other listeners (sipgo's `Upgrade` fails and closes — verify it
  does not log noisily or leak).
- **WSS handshake failure**: should be audit-logged with peer + sanitized reason and
  no certificate bytes — reuse the `auditConn` discipline; confirm it composes with
  the WS upgrade ordering (TLS handshake precedes WS upgrade).
- **Idle flow with no client pings**: if AC7 relies only on client-initiated pings,
  a silent client still times out — argues for server-initiated ping or documenting
  the dependency on STORY-018.
- **One slow/blocking WS upgrade**: must not stall accepts on UDP/TLS/other WS
  (non-functional). sipgo accepts per-connection; confirm the upgrade runs off the
  accept loop.

### Technical Risks
- **AC4 not enforced by stock sipgo** (`sip/transport_ws.go:116-145`): sipgo
  advertises `sip` but does not reject clients that omit it. *Impact:* AC4 fails as
  written. *Mitigation:* thin upgrade guard inspecting the client
  `Sec-WebSocket-Protocol` header (preferred), or upstream/forked `Upgrader.Protocol`
  negotiation.
- **AC7 ping/pong absent in stock sipgo v1.4.0** (`sip/transport_ws.go:408-413`
  commented out; inbound Ping discarded at `:393`): *Impact:* AC7 not satisfiable
  with the dependency as-is — the single biggest risk in the story. *Mitigation:*
  connection wrapper answering Ping→Pong before sipgo reads; or fork/patch; or
  rescope AC7 onto TCP keepalive + STORY-018 CRLF keep-alive (requires AC
  renegotiation).
- **Per-listener TLS config plumbing**: the engine currently holds a single
  `tlsServerConf` derived from `cfg.TLS.Listen`. Adding WSS means resolving and
  building an independent server config per WSS listener and threading it through
  `Run`'s errgroup — a small but real refactor of the listener-startup code.
- **Binary frames discarded**: if any client (non-browser) sends binary-framed SIP,
  it is silently dropped by sipgo. Low likelihood for the named clients; surface it
  so it is a conscious scope decision, not a latent bug.
- **Concurrency/lifecycle**: WS/WSS goroutines join the same `errgroup`; a bind
  failure must cancel siblings (fail-fast) and clean ctx-cancel must return nil —
  must match the existing UDP/TLS shutdown semantics (`serveTLS` is the template).
  Run under `go test -race` per the definition of done.

### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Webphone over WS, signaling routed normally | Yes | sipgo `ListenAndServe(ctx,"ws",addr)`; advertises `sip`; routes via existing handlers |
| AC2 | Webphone over WSS in production | Yes | `ListenAndServeTLS(ctx,"wss",addr,conf)` with `*tls.Config` from `tlsprov`; per-WSS context build at `New` (fail-fast) |
| AC3 | Routed identically to other transports | Yes | B2BUA handlers are transport-agnostic — holds by construction; add a test proving a ws-originated call bridges like UDP |
| AC4 | Sequencer negotiates the `sip` subprotocol (rescoped) | Yes | sipgo advertises/negotiates `sip` by default; assert the negotiated subprotocol in the upgrade response |
| AC5 | WS and existing listeners active simultaneously | Yes | Add `g.Go` siblings in the `Run` errgroup, alongside UDP/TLS |
| AC6 | Omitting WS config opens no WS listener | Yes | Mirror the `cfg.TLS.Listen != ""` opt-in gate; empty `ws`/`wss` → no goroutine |

**Coverage summary:** all 6 (post-decision) ACs directly addressable on the existing
inbound-TLS pattern. The two prior risks are closed by the **use-sipgo-as-is**
decision (see Decision Log): non-`sip` rejection dropped, ping/pong delegated to
TCP + STORY-018 CRLF keep-alive. No open blockers for REASONS Canvas.
