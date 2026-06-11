# Story Decomposition: SIP over WebSocket support

> These files (`[User-story-17]` … `[User-story-21]`) extend the **module-001**
> decomposition of the sequencer with **SIP over WebSocket** transport and the
> **WebRTC media bridge** it requires, derived from
> `requirements/support-websocket-sip.md`. Same pattern as the TLS module
> (`[User-story-12]` … `[User-story-16]`): additional capabilities of the existing
> B2BUA, numbered `STORY-001-017` … `STORY-001-021`.

## INVEST Analysis

### Abstract Task
**Feature Name:** SIP over WebSocket (signaling transport) + WebRTC media bridge
(DTLS-SRTP ↔ plain RTP, ICE-lite)

**Analysis Dimensions**
- **Core Responsibility:** Let browser SIP clients (jssip / sip.js webphones)
  connect to the sequencer over WebSocket and place calls, with the sequencer
  unchanged as a proxy / B2BUA. Two planes are involved: a new **signaling
  transport** (SIP over WebSocket, peer to UDP/TCP/TLS, including the registration
  / outbound-flow handling browsers need) and a new **media plane** (the webphone
  leg is always WebRTC — DTLS-SRTP + ICE — so it must be terminated and bridged to
  the plain-RTP leg on the other side). **No transcoding** — codecs are negotiated
  end to end and pass through unchanged.
- **Primary Operations:** open WS/WSS inbound listeners and negotiate the `sip`
  subprotocol; extract SIP messages from WebSocket frames and route them through
  the existing stack; handle SIP Outbound registration flows (reg-id, instance-id,
  single long-lived flow) and insert Path so requests route back through the edge;
  hold flows open with WS ping/pong and CRLF keep-alives; answer ICE-lite and
  accept trickled candidates; terminate DTLS-SRTP and bridge it to plain RTP
  without converting codecs.
- **Key Constraints:** WebSocket is *just a transport* — SIP parsing and routing
  are identical to the other transports; WS/WSS listen in parallel with the
  existing listeners and are opt-in/additive (a config with no WebSocket keys
  behaves exactly as today); seamless client migration (existing jssip / sip.js
  integrations work with no code changes); **no transcoding**; the server is a
  public ICE-lite anchor (host candidates only, no TURN, no full ICE); per-leg
  media security must be a configurable property of each leg (future-proof for an
  SRTP↔SRTP proxy later), not hardcoded to "webphone = SRTP, other = RTP".
- **Technical Complexity:** High (WebSocket framing of SIP, RFC 5626 outbound
  flows, DTLS-SRTP handshake, ICE-lite, trickle ICE, secure↔plain media bridge).
- **Business Complexity:** Medium (registration/flow model, backward
  compatibility, seamless client migration).

### INVEST Evaluation (whole feature)
- ❌ **Independent** — spans a signaling transport, a registration/flow layer, and
  a whole new media plane (DTLS-SRTP, ICE, bridge).
- ✅ **Negotiable**
- ✅ **Valuable** — browser webphones can place calls through the sequencer.
- ❌ **Small** — multi-week.
- ✅ **Testable**

**Conclusion:** Needs splitting.

### Split Strategy
Split **by capability** (not by technical layer). 5 stories, layered so each
delivers independent, testable value and the next builds on a stable boundary.
Signaling plane first (017–018), then the media plane (019–021):
1. `[STORY-001-017]` WebSocket SIP signaling transport — WS/WSS inbound listeners,
   `sip` subprotocol negotiation, frame ↔ SIP message handling, WS ping/pong
   keep-alive, parallel with existing transports (this file)
2. `[STORY-001-018]` SIP Outbound (RFC 5626) + Path (RFC 3327) over WebSocket —
   registration flow handling, reg-id / instance-id, single long-lived flow,
   connection reuse, Path so requests route back, CRLF keep-alive
3. `[STORY-001-019]` WebRTC media leg — DTLS-SRTP termination + ICE-lite
   (host-candidate, answer/validate) + rtcp-mux, with per-leg media security as a
   configurable property
4. `[STORY-001-020]` Trickle ICE (RFC 8838) — accept trickled candidates and
   end-of-candidates on the webphone media leg
5. `[STORY-001-021]` DTLS-SRTP ↔ plain RTP media bridge — bridge the secured
   webphone leg to the plain-RTP leg on the other side, codecs passing through
   unchanged (no transcoding)

---

## [STORY-001-017] WebSocket SIP signaling transport

### Background
Web-based SIP clients (webphones built with jssip or sip.js) speak SIP over
WebSocket (RFC 7118); they cannot use the UDP/TCP/TLS listeners the sequencer
exposes today. This story adds WebSocket as another inbound transport, peer to the
existing ones: an unencrypted **WS** endpoint (development/testing only) and an
encrypted **WSS** endpoint (production). On connect, the sequencer negotiates the
`sip` WebSocket subprotocol (`Sec-WebSocket-Protocol: sip`); thereafter each
WebSocket text frame carries a SIP message that is parsed and routed
**exactly as on the other transports** — the sequencer stays a proxy / B2BUA and
gains no WebSocket-specific signaling behavior. The WebSocket listeners run in
parallel with the existing listeners; a config with no WebSocket keys opens no
WebSocket listener at all.

> **Implementation note (decided):** the sequencer uses the underlying SIP
> library's (sipgo) WebSocket transport **as-is**, with no fork or wrapper. Two
> consequences are baked into the ACs below: (1) the transport *advertises and
> negotiates* the `sip` subprotocol but does not actively *reject* a client that
> omits it — acceptable because jssip / sip.js always offer `sip`; (2) the
> transport does not handle WebSocket ping/pong, so holding an idle flow open is
> provided by TCP-level keep-alive now and the RFC 5626 CRLF keep-alive in
> `[STORY-001-018]`, not by WebSocket ping/pong.

Key points:
- Business value: browser webphones can reach the sequencer with no change to
  their application; SIP semantics are preserved end to end.
- First place WebSocket terminates on the wire — the foundation every other
  WebSocket story builds on.
- Fully backward compatible and opt-in — existing configs and clients are
  unaffected.

### Business Value
- Provide jssip / sip.js webphones a standards-based way (RFC 7118) to reach the
  sequencer without changing their application.
- Support a migration path — WebSocket and the existing UDP/TCP/TLS listeners run
  simultaneously, so clients adopt WebSocket at their own pace.
- Preserve standard SIP semantics — WebSocket is just the transport; routing and
  B2BUA behavior are unchanged.

### Dependencies and Assumptions
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

### Scope In
- Open an inbound **WS** listener and an inbound **WSS** listener (each optional)
  on their configured addresses, alongside the existing UDP/TCP/TLS listeners
  (parallel, separate ports).
- Complete the WebSocket upgrade handshake and negotiate the `sip` subprotocol
  (`Sec-WebSocket-Protocol: sip`).
- Extract a SIP message from each inbound WebSocket text frame and hand it to the
  existing parsing/routing stack; frame outbound SIP messages back over the same
  WebSocket connection.

### Scope Out
- SIP Outbound registration flows, reg-id / instance-id, connection reuse, Path,
  and RFC 5626 CRLF keep-alive — `[STORY-001-018]`.
- All media-plane work (DTLS-SRTP, ICE-lite, trickle ICE, the RTP bridge) —
  `[STORY-001-019]` … `[STORY-001-021]`.
- Building the TLS material that secures WSS — reused from the TLS module.
- WebSocket ping/pong keep-alive — the chosen library does not handle it; idle-flow
  keep-alive is provided by TCP keep-alive and the RFC 5626 CRLF keep-alive in
  `[STORY-001-018]`.
- Actively rejecting a client that does not offer the `sip` subprotocol — the
  transport advertises/negotiates `sip` but does not enforce rejection; jssip /
  sip.js always offer `sip`.
- Binary WebSocket frames — only text frames carry SIP (matches jssip / sip.js).
- Certificate reload without restart (future consideration).

### Acceptance Criteria

#### AC1: A webphone connects over WS and its signaling is routed normally
**Given** an inbound WS listener on `0.0.0.0:8080` and a jssip webphone configured
to use it
**When** the webphone opens the WebSocket connection and sends a `REGISTER` /
`INVITE`
**Then** the WebSocket upgrade completes with the `sip` subprotocol negotiated, and
the SIP signaling is processed exactly as the same request would be over UDP/TCP.

#### AC2: A webphone connects over WSS in production
**Given** an inbound WSS listener on `0.0.0.0:8443` using a valid certificate
**When** a sip.js webphone connects over `wss://`
**Then** the connection is established over TLS, the `sip` subprotocol is
negotiated, and signaling proceeds over the encrypted WebSocket exactly as over WS.

#### AC3: SIP over WebSocket is routed identically to other transports
**Given** a webphone on WebSocket and a callee reachable via the configured
sequence / next hop
**When** the webphone places a call
**Then** the call is bridged by the B2BUA exactly as a call arriving on UDP/TCP/TLS
would be — the transport changes nothing about routing or dialog handling.

#### AC4: The sequencer negotiates the `sip` subprotocol
**Given** the WS listener on `0.0.0.0:8080`
**When** a jssip / sip.js webphone opens a WebSocket connection offering
`Sec-WebSocket-Protocol: sip`
**Then** the upgrade response selects `sip` as the negotiated subprotocol and SIP
signaling proceeds over the connection.

#### AC5: WebSocket and existing listeners are active simultaneously
**Given** the existing UDP listener on `0.0.0.0:5060` and a WS listener on
`0.0.0.0:8080`
**When** the process is running
**Then** a UDP SIP client on 5060 is served as before **and** a webphone on 8080 is
served over WebSocket — both transports are active at once, neither replacing the
other.

#### AC6: Omitting the WebSocket config opens no WebSocket listener
**Given** a config with no `ws` / `wss` listener keys
**When** the process starts
**Then** only the existing listeners are opened; no WebSocket port is bound and
behavior is exactly as today.

#### Non-Functional Expectations
- A slow or failed WebSocket upgrade from one client must not block acceptance of
  connections on the other listeners.
- A SIP message carried over WebSocket must be parsed by the same code path as the
  other transports — no WebSocket-specific signaling semantics.
