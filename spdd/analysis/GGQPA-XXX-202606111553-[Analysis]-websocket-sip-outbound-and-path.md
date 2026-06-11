# SPDD Analysis: SIP Outbound (RFC 5626) + Path (RFC 3327) over WebSocket (STORY-001-018)

## Original Business Requirement

> Source: `requirements/[User-story-18]websocket-sip-outbound-and-path.md` (story
> 2/5 of the SIP-over-WebSocket decomposition). Reproduced verbatim below.

### [STORY-001-018] SIP Outbound (RFC 5626) + Path (RFC 3327) over WebSocket

#### Background
A browser webphone cannot be reached at a routable IP/port the way a hardware
phone can — it lives behind a single, long-lived WebSocket connection it opened
outbound to the sequencer. jssip and sip.js therefore register using **SIP
Outbound** (RFC 5626): they send `REGISTER` with `;ob`, a **reg-id**, and a
**`+sip.instance` instance-id**, all over one persistent flow. For an inbound
request to reach that webphone later, the sequencer must remember the flow the
registration arrived on and route the request back over it — which requires
inserting a **Path** header (RFC 3327) at the WebSocket edge so the registrar's
binding records the route back through this sequencer. The flow is held open with
RFC 5626 CRLF keep-alives (in addition to WebSocket ping/pong from
`[STORY-001-017]`), and subsequent requests reuse the existing flow rather than
opening a new connection.

Key points:
- Business value: webphones stay reachable for inbound calls through their single
  outbound WebSocket flow, which is the only way a browser can receive a call.
- Builds directly on the WebSocket transport (`[STORY-001-017]`) — turns a
  connected webphone into a registerable, reachable endpoint.
- Required for seamless migration — jssip / sip.js use Outbound + Path by default,
  so they must work with no client code change.

#### Business Value
- Provide webphones a way to remain reachable for inbound calls over a single
  long-lived WebSocket flow (the browser cannot accept inbound connections).
- Support connection reuse so a webphone keeps one flow for registration, calls,
  and in-dialog requests — no reconnect churn.
- Enable seamless client migration — existing jssip / sip.js Outbound + Path
  behavior works unchanged.

#### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-017]` (WebSocket transport carrying SIP).
- **Data assumptions:** jssip / sip.js register with `;ob`, a reg-id, and an
  instance-id over the WebSocket flow; a registrar / next hop records the resulting
  binding.
- **Integration points:** Browser SIP clients (jssip, sip.js); the upstream
  registrar / next hop that stores bindings and later sends inbound requests.
- **Business constraints:** Standard SIP semantics preserved; existing clients work
  with no code changes; the webphone's single outbound flow is the only path back
  to it.

#### Scope In
- Handle SIP Outbound registrations over WebSocket: recognize `;ob`, the reg-id,
  and the `+sip.instance` instance-id, and associate them with the WebSocket flow
  the registration arrived on.
- Insert a Path header at the WebSocket edge so the binding routes inbound requests
  back through this sequencer and onto the correct flow.
- Route an inbound request addressed to a registered webphone back over its stored
  outbound flow.
- Reuse the existing flow for subsequent requests from the same webphone instead of
  opening a new connection.
- Maintain the flow with RFC 5626 CRLF keep-alives.

#### Scope Out
- The WebSocket transport itself and WS ping/pong keep-alive — `[STORY-001-017]`.
- All media-plane work — `[STORY-001-019]` … `[STORY-001-021]`.
- Registrar / location-service storage semantics beyond what is needed to route back
  over the flow (the sequencer is a proxy/B2BUA, not the authoritative registrar).

#### Acceptance Criteria
- **AC1:** An Outbound registration establishes a reachable flow (reg-id /
  instance-id associated with the arriving WebSocket flow).
- **AC2:** An inbound call is routed back over the webphone's flow via Path — the
  phone rings without the webphone opening a new connection.
- **AC3:** Subsequent requests reuse the existing flow (no new connection).
- **AC4:** CRLF keep-alive holds the flow open.
- **AC5:** A webphone re-registers over a new flow after reconnect; inbound now
  routes over the new flow.
- **AC6:** Existing jssip / sip.js clients work with no code change.
- **Non-Functional:** The route back must depend only on the stored flow and Path,
  so a NAT'd webphone with no reachable address is still reachable.

---

## Decision Log
- **`next_hop` supports RFC 3327 Path** (decided). The upstream registrar behind
  `next_hop` honors the inserted Path and reflects it into the inbound request's
  Route. Inbound flow routing (AC2) depends on this; no fallback for a non-Path
  registrar is in scope.
- **AC4 rescoped** (decided): the flow is held open by the persistent ws/TCP
  connection, not by a server-answered keep-alive. Per the standing "use sipgo as-is"
  decision (STORY-001-017), the sequencer answers no CRLF/ws keep-alive; clients may
  still send CRLF keep-alives harmlessly.
- **Flow token is HMAC-signed** (constraint, decided): a forged Route must not route
  the sequencer to an arbitrary internal address.
- **Inbound-to-webphone is signaling only** here ("phone rings"); RTP/WebRTC media
  bridging is deferred to STORY-001-019..021.

## Domain Concept Identification

### Existing Concepts (from codebase)
- **Engine** (`internal/b2bua/engine.go`): owns the sipgo `UserAgent`/`Server`/
  `Client` and binds method handlers in `Run`. The sipgo `Server` exposes
  `OnRegister` (confirmed in dep `server.go:307`), so REGISTER can be intercepted
  cleanly rather than falling through `OnNoRoute`.
- **`proxyUnmanaged`** (`internal/b2bua/proxy.go`): today REGISTER is *not* handled
  — it falls to `OnNoRoute` → `proxyUnmanaged`, a stateless blind forward to
  `cfg.NextHop` that adds/strips a single proxy `Via`. It does **no** RFC 3261
  loose-routing, no `Path`, no flow tracking. It is the closest existing pattern
  (clone, Max-Forwards, `SetDestination`, `TransactionRequest`, relay responses)
  this story will extend/parallel.
- **Header ownership model** (`internal/b2bua/headers.go`): `requestOwnedHeaders` /
  `responseOwnedHeaders` define what the B2BUA rewrites vs. relays; `route` /
  `record-route` are already in the owned set. `Path` is a new header to manage at
  the edge.
- **sipgo transport connection reuse** (dep `sip/transport_layer.go`):
  `connectionReuse` defaults to **true**; outbound routing resolves a connection via
  `GetConnection(<addr>)`, reusing an existing inbound connection when the request
  destination equals the peer's source address. This is the mechanism that makes
  "route back over the existing ws flow" possible without any new transport code —
  set the destination to the webphone's stored source address and sipgo returns the
  cached ws connection.
- **B2BUA call/bridge path** (`bridge.go`/`call.go`): the existing flow is
  caller → sequence apps → `next_hop`, anchoring RTP. An inbound call *to* a
  webphone reverses roles and is **not** modeled today.
- **Next hop** (`cfg.NextHop`): the single terminating hop the sequencer forwards
  unmanaged traffic to; assumed to be (or to front) the authoritative registrar.

### New Concepts Required
- **Flow**: the live WebSocket connection a webphone opened, identified for routing
  by the peer's source address (host:port + ws/wss transport) as seen by the
  sequencer. The unit that "route back over the flow" targets.
- **Flow token (RFC 5626)**: an opaque token the sequencer places in the user-part
  of the **Path** URI it inserts on REGISTER, and recovers from the top **Route** of
  an inbound request, to map a binding back to a Flow. Encodes (or references) the
  Flow's source address/transport.
- **Outbound registration edge**: the REGISTER-handling behavior — recognize `;ob` /
  `reg-id` / `+sip.instance`, insert Path with the flow token, forward to the
  registrar, relay the response — without the sequencer becoming the authoritative
  registrar.
- **Inbound flow routing**: loose-route-style handling for a request whose top Route
  is this sequencer's Path — pop the Route, decode the flow token, set the
  destination to the Flow's source address, forward (sipgo reuses the ws
  connection).
- **Instance/reg-id association** (optional state): a small map from
  `+sip.instance` (+ reg-id) to Flow, owned by one component, supporting re-register
  flow replacement (AC5) and cleanup on disconnect.

### Key Business Rules
- **The flow is the only path back**: an inbound request reaches a webphone solely
  via its stored Flow + Path; never via a routable address in its Contact (governs
  inbound routing and the flow-token design).
- **One flow per webphone, reused**: registration, outbound calls, and in-dialog
  requests all traverse the same connection; the sequencer never dials the webphone
  (governs reliance on sipgo `connectionReuse`).
- **Re-register replaces the flow**: a reconnect + re-register with the same
  instance-id/reg-id rebinds to the new Flow; the stale Flow stops being routed
  (governs AC5 and flow lifecycle).
- **Proxy, not registrar**: the sequencer inserts Path and routes; the upstream
  registrar stores and honors the binding. Requires the registrar to support RFC
  3327 Path.
- **Standard SIP / no client change**: behavior must match what jssip / sip.js
  emit by default (governs strict RFC 5626/3327 conformance at the edge).

## Strategic Approach

### Solution Direction
Add a thin **registration edge** on top of the existing proxy, leaning on sipgo's
built-in connection reuse for the hard part (sending back over the ws flow):

`REGISTER over ws → OnRegister handler: detect Outbound (;ob/reg-id/+sip.instance),
record Flow = peer source addr, insert Path (URI = this sequencer + flow token),
forward to next_hop/registrar, relay response. ── Inbound INVITE from registrar
carrying Route = our Path → recognize self, pop Route, decode flow token → source
addr, SetDestination(addr), forward; sipgo connectionReuse returns the cached ws
connection → webphone rings.`

For AC3, nothing extra is needed: because the webphone holds one ws connection and
sipgo reuses connections by address, subsequent requests naturally ride the same
flow. The new code is concentrated in (a) the REGISTER/Path edge and (b) the
inbound Route→flow routing — both modeled on `proxyUnmanaged`'s clone/forward/relay
shape.

### Key Design Decisions
- **REGISTER handling — `OnRegister` vs. branching `proxyUnmanaged`**: sipgo exposes
  `OnRegister`. Recommendation: a dedicated REGISTER handler. Rationale: keeps the
  Path/flow concern out of the generic proxy; reveals intent; `proxyUnmanaged` stays
  a simple blind forward.
- **Flow token — stateless (encode addr) vs. stateful (registry)**: a stateless,
  **signed** token encoding source addr+transport needs no shared state and survives
  as long as the connection's address is valid; a registry adds lifecycle control
  (AC5 cleanup, liveness) but introduces owned mutable state. Recommendation: start
  stateless+signed for routing, plus a **small instance→flow map** only if AC5/cleanup
  proves to need it — keep state minimal (AGENTS.md). Either way the token MUST be
  integrity-protected (see risks).
- **Inbound routing — full RFC 3261 loose-route vs. minimal Path recognition**: the
  sequencer only needs to recognize *its own* Path/Route, pop it, and forward to the
  flow. Recommendation: implement the minimal self-Route handling, not a general
  loose-route proxy (YAGNI) — but conform to RFC 3261 §16.6 for the headers it does
  touch.
- **Inbound-to-webphone — proxy now, bridge later**: this story is signaling only
  (AC2 = "phone rings"). Recommendation: route the inbound INVITE to the flow as a
  proxy forward; defer RTP anchoring / WebRTC media bridging to STORY-019..021. This
  must be stated explicitly as the scope boundary.
- **CRLF keep-alive (AC4) — given "use sipgo as-is"**: sipgo's parser tolerates a
  leading CRLF but emits no CRLF pong (and, per STORY-017, no ws pong). Recommendation:
  treat flow-open as provided by the persistent ws/TCP connection; if AC4 strictly
  requires the server to answer keep-alives, **rescope it** the same way STORY-017's
  AC7 was rescoped (decision required — mirrors the prior precedent).
- **Testing (AGENTS.md)**: drive with a **real** ws SIP client registering with
  `;ob`/reg-id/instance and a **real fake upstream registrar** that records the Path
  and replays an inbound INVITE; assert it arrives on the original ws flow. No mocks
  of internal code.

### Alternatives Considered
- **Make the sequencer the authoritative registrar (store bindings, answer
  REGISTER 200 itself)**: rejected — the requirement says proxy/B2BUA, registrar is
  upstream; storing bindings duplicates the registrar and breaks "standard SIP
  semantics preserved."
- **Stateful flow registry keyed by Contact/AOR instead of flow token in Path**:
  rejected as the primary mechanism — without Path the registrar cannot record the
  route back, so a flow token in Path is required for RFC-conformant, client-no-change
  behavior; a registry is at most a supplement.
- **Dial the webphone on inbound (open a new connection)**: impossible — a browser
  accepts no inbound connections; the whole story exists to avoid this.

## Risk & Gap Analysis

### Requirement Ambiguities
- **Registrar location**: "the registrar / next hop" — is `cfg.NextHop` itself the
  registrar, or does it route onward to one? Path routing only works if the
  registrar honors RFC 3327 and reflects the Path into inbound Route. Needs
  confirmation of the upstream's Path support.
- **AC4 keep-alive semantics**: must the server *answer* CRLF keep-alives, or only
  *tolerate* them while the connection stays open by other means? Stock sipgo does
  not answer them (mirrors STORY-017's ping/pong gap).
- **Inbound-to-webphone media**: AC2 says "rings" (signaling). Whether the inbound
  call anchors/bridges media here or defers to STORY-019..021 is unstated — assumed
  deferred.
- **Flow identity under NAT**: the Flow is keyed by source address; is the same
  webphone allowed multiple flows (multiple reg-ids/tabs)? RFC 5626 supports several
  registration flows per instance; scope likely one, but unstated.

### Edge Cases
- **Inbound request after the flow dropped**: the cached connection is gone; routing
  to the stale address must fail gracefully (e.g. 480/408) and not dial a new
  connection or hang.
- **Re-register from a new flow while the old is still open** (AC5): both flows
  briefly exist; the binding must follow the newest Path; the old flow must be
  retired without misrouting.
- **Flow token from a foreign/forged Route**: an attacker-supplied Route could try
  to make the sequencer forward to an arbitrary internal address — the token must be
  rejected unless integrity-valid (security risk below).
- **REGISTER without `;ob`/reg-id (plain registration over ws)**: must still work
  (insert Path? or plain-forward?) — define behavior so non-Outbound clients aren't
  broken.
- **Response relay for REGISTER**: 401/407 challenge round-trips must pass through
  unchanged (auth lives end to end), like `proxyUnmanaged` already does.

### Technical Risks
- **No `Path` header type in sipgo**: must construct/parse Path as a generic header
  and parse the top Route ourselves. *Impact:* low–medium; manual header work, must
  conform to RFC 3327/3261. *Mitigation:* small, well-tested header helpers.
- **No loose-routing today**: `proxyUnmanaged` is a blind forward; inbound flow
  routing (recognize self-Route, pop, forward to flow) is new control flow.
  *Impact:* medium. *Mitigation:* minimal self-Route handling only.
- **Connection-reuse assumptions**: routing back depends on sipgo keying the cached
  ws connection by the peer source address and `connectionReuse` staying true (it is
  the default). *Impact:* high if the assumption breaks (e.g. address churn).
  *Mitigation:* a behavioral test that an inbound request truly lands on the original
  ws connection, not a new dial.
- **Flow-token forgery / SSRF-style internal routing**: an unauthenticated token
  that maps to an arbitrary destination is a security hole. *Impact:* high.
  *Mitigation:* HMAC-sign the token (or validate against a known-flows set); never
  forward to an address that does not correspond to a live, recorded flow.
- **AC4 not answerable with stock sipgo**: no CRLF/ws keep-alive responder. *Impact:*
  medium — flow still held by the persistent connection, but the literal AC may need
  rescoping (decision, per STORY-017 precedent).
- **Concurrency/lifecycle**: any instance→flow map is shared mutable state touched by
  the register path and the inbound-routing path; must be owned by one component
  behind a narrow interface and race-free (`go test -race`).

### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | Outbound register establishes a reachable flow | Yes | `OnRegister` handler; parse `;ob`/reg-id/`+sip.instance`; record flow = source addr; insert Path |
| AC2 | Inbound routed back over the flow via Path | Yes (new logic) | Recognize self-Route, decode flow token, `SetDestination(srcAddr)`, forward; relies on sipgo `connectionReuse` |
| AC3 | Subsequent requests reuse the existing flow | Yes | Falls out of sipgo connection reuse (one ws conn keyed by source addr) — add a confirming test |
| AC4 | Flow stays open while the connection persists (rescoped) | Yes | Resolved per Decision Log: flow held by the persistent ws/TCP connection; no server-answered keep-alive (sipgo as-is) |
| AC5 | Re-register over a new flow after reconnect | Yes | New REGISTER carries fresh Path/flow token (new source addr); retire stale flow; needs lifecycle handling |
| AC6 | Existing jssip / sip.js clients work unchanged | Yes | Requires strict RFC 5626/3327 conformance at the edge; depends on the upstream registrar honoring Path |
| NFE | Route depends only on flow + Path | Yes | This is precisely the flow-token-in-Path design; no reliance on Contact reachability |

**Coverage summary:** all 7 addressable post-decision. **AC2/AC5** carry the real
implementation work (Path edge + inbound flow routing + flow lifecycle). The two
prior gates are closed (see Decision Log): `next_hop` honors RFC 3327 Path; AC4 is
rescoped to the persistent connection. The flow token MUST be HMAC-signed. No open
blockers for REASONS Canvas.
