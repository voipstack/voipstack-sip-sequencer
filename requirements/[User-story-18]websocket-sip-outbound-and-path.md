# Story Decomposition: SIP over WebSocket support (story 2/5)

> Part of the WebSocket decomposition. Module overview and INVEST analysis in
> `[User-story-17]websocket-sip-signaling-transport.md`. Derived from
> `requirements/support-websocket-sip.md`.

---

## [STORY-001-018] SIP Outbound (RFC 5626) + Path (RFC 3327) over WebSocket

### Background
A browser webphone cannot be reached at a routable IP/port the way a hardware
phone can — it lives behind a single, long-lived WebSocket connection it opened
outbound to the sequencer. jssip and sip.js therefore register using **SIP
Outbound** (RFC 5626): they send `REGISTER` with `;ob`, a **reg-id**, and a
**`+sip.instance` instance-id**, all over one persistent flow. For an inbound
request to reach that webphone later, the sequencer must remember the flow the
registration arrived on and route the request back over it — which requires
inserting a **Path** header (RFC 3327) at the WebSocket edge so the registrar's
binding records the route back through this sequencer. The flow is held open by the
persistent WebSocket/TCP connection, and subsequent requests reuse that existing
flow rather than opening a new connection.

> **Decided assumptions / limitations:**
> - **`next_hop` supports RFC 3327 Path** — the upstream registrar behind `next_hop`
>   honors the Path header the sequencer inserts and reflects it into the inbound
>   request's Route. Inbound routing back to the webphone depends on this.
> - **sipgo used as-is** (see [STORY-001-017]) — sipgo answers no RFC 5626 CRLF
>   keep-alive (nor WebSocket ping/pong). The flow stays open via the persistent
>   ws/TCP connection itself, not a server-sent keep-alive response. AC4 is scoped
>   accordingly.
> - **Flow token must be integrity-protected** (HMAC-signed) so a forged Route
>   cannot make the sequencer forward to an arbitrary internal address.

Key points:
- Business value: webphones stay reachable for inbound calls through their single
  outbound WebSocket flow, which is the only way a browser can receive a call.
- Builds directly on the WebSocket transport (`[STORY-001-017]`) — turns a
  connected webphone into a registerable, reachable endpoint.
- Required for seamless migration — jssip / sip.js use Outbound + Path by default,
  so they must work with no client code change.

### Business Value
- Provide webphones a way to remain reachable for inbound calls over a single
  long-lived WebSocket flow (the browser cannot accept inbound connections).
- Support connection reuse so a webphone keeps one flow for registration, calls,
  and in-dialog requests — no reconnect churn.
- Enable seamless client migration — existing jssip / sip.js Outbound + Path
  behavior works unchanged.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-017]` (WebSocket transport carrying SIP).
- **Data assumptions:** jssip / sip.js register with `;ob`, a reg-id, and an
  instance-id over the WebSocket flow; a registrar / next hop records the
  resulting binding.
- **Integration points:** Browser SIP clients (jssip, sip.js); the upstream
  registrar / next hop that stores bindings and later sends inbound requests.
- **Business constraints:** Standard SIP semantics preserved; existing clients
  work with no code changes; the webphone's single outbound flow is the only path
  back to it.

### Scope In
- Handle SIP Outbound registrations over WebSocket: recognize `;ob`, the reg-id,
  and the `+sip.instance` instance-id, and associate them with the WebSocket flow
  the registration arrived on.
- Insert a Path header at the WebSocket edge so the binding routes inbound
  requests back through this sequencer and onto the correct flow.
- Route an inbound request addressed to a registered webphone back over its stored
  outbound flow.
- Reuse the existing flow for subsequent requests from the same webphone instead of
  opening a new connection.
- Keep the flow usable for as long as its WebSocket/TCP connection persists (no
  server-sent keep-alive; tolerate client CRLF keep-alives).

### Scope Out
- The WebSocket transport itself and WS ping/pong keep-alive — `[STORY-001-017]`.
- All media-plane work — `[STORY-001-019]` … `[STORY-001-021]`.
- Registrar / location-service storage semantics beyond what is needed to route
  back over the flow (the sequencer is a proxy/B2BUA, not the authoritative
  registrar).

### Acceptance Criteria

#### AC1: An Outbound registration establishes a reachable flow
**Given** a jssip webphone connected over WebSocket
**When** it sends `REGISTER` with `;ob`, `reg-id=1`, and a `+sip.instance`
instance-id
**Then** the registration succeeds and the sequencer associates that reg-id /
instance-id with the WebSocket flow it arrived on.

#### AC2: An inbound call is routed back over the webphone's flow via Path
**Given** a webphone registered as in AC1 and a Path inserted at the WebSocket edge
**When** an inbound `INVITE` for that webphone arrives from the upstream side
**Then** the sequencer routes it back through the recorded Path onto the webphone's
existing outbound WebSocket flow, and the webphone's phone rings — without the
webphone having opened any new connection.

#### AC3: Subsequent requests reuse the existing flow
**Given** a webphone with an established outbound flow
**When** it sends further requests (re-REGISTER, a new INVITE, in-dialog requests)
**Then** they travel over the same flow and the sequencer does not open a new
connection to the webphone.

#### AC4: The flow stays open while the connection persists
**Given** a registered webphone whose WebSocket flow is otherwise idle
**When** the idle period elapses while the WebSocket/TCP connection remains open
**Then** the flow is still usable and the webphone remains reachable for inbound
calls. (The sequencer relies on the persistent connection; per the as-is sipgo
decision it does not answer RFC 5626 CRLF keep-alives — clients may still send them
harmlessly.)

#### AC5: A webphone re-registers over a new flow after reconnect
**Given** a webphone whose WebSocket flow has dropped (network loss)
**When** the webphone reconnects and re-registers with the same instance-id and
reg-id
**Then** the sequencer associates the binding with the new flow, and inbound calls
now route over the new flow.

#### AC6: Existing jssip / sip.js clients work with no code change
**Given** an unmodified jssip or sip.js webphone configured only with the
sequencer's WebSocket URL and credentials
**When** it registers and places / receives a call
**Then** registration, outbound calls, and inbound calls all work, with no
client-side code change required.

#### Non-Functional Expectations
- The route back to a webphone must depend only on its stored flow and Path, so a
  webphone behind NAT with no reachable address is still reachable for inbound
  calls.
