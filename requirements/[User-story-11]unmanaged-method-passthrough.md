# [STORY-001-011] Transparent pass-through of unmanaged SIP methods to PBX

> Story 011 of the module-001 decomposition of `PRD.md`. See `[User-story-1]` for the
> shared INVEST analysis and split strategy. Added because the sequencer is deployed
> **in front of a PBX** — the PBX stays the registrar/feature server, so non-call SIP
> traffic must reach it transparently.

### Background
The sequencer sits inline in front of a PBX; SIP endpoints point at the sequencer as their
proxy. The sequencer is a B2BUA for **calls** (INVITE dialogs → application chain → PBX),
but endpoints also REGISTER, subscribe to presence/voicemail, send messages, and ping with
OPTIONS — all owned by the PBX, not the sequencer. If the sequencer dropped or rejected
those, phones could not register or receive presence/MWI. This story makes the sequencer
**transparently forward every SIP method it does not manage to the terminating next-hop
(PBX)**, unmodified, so the PBX keeps working as before. These methods never enter the
application chain — applications process calls only.

Key points:
- Business value: the sequencer can be dropped in front of an existing PBX without breaking
  registration, presence, messaging, or keepalives.
- Keeps the B2BUA focused on calls; everything else is a thin transparent forward.
- Needed because a call-only inline element would otherwise blackhole non-call SIP.

### Business Value
- Enable inline deployment in front of an existing PBX with no loss of PBX features.
- Preserve endpoint registration, presence/MWI (SUBSCRIBE/NOTIFY), messaging (MESSAGE),
  and keepalives (OPTIONS) by forwarding them to the PBX.
- Keep the application chain call-only — apps are not bothered with non-call traffic.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-002]` (engine/SIP server exists); `[STORY-001-001]`
  (next-hop from config).
- **Data assumptions:** the PBX at `next_hop` is the real registrar / feature server and
  accepts forwarded requests; plain SIP over UDP (per story 002).
- **Integration points:** SIP endpoints, terminating PBX.
- **Business constraints:** the sequencer modifies nothing in the forwarded message beyond
  what a stateless proxy must (Via/Max-Forwards); no Record-Route, no Contact rewriting v1.

### Scope In
- Classify each inbound SIP request: **managed** (call methods — `INVITE`, `ACK`, `CANCEL`,
  `BYE`, and the mid-call methods owned by the B2BUA) vs **unmanaged** (everything else).
- **Stateless-proxy** every unmanaged method to the `next_hop` (PBX): add a `Via`, decrement
  `Max-Forwards`, forward; route the PBX's responses back to the originator.
- Forward in whichever direction the sequencer receives the request (endpoint→PBX, and any
  request the PBX sends that arrives at the sequencer).

### Scope Out
- Record-Route insertion / staying in the path for in-dialog follow-ups (subscribe dialogs)
  — out of scope v1 (a stateless forward without Record-Route).
- Contact / registration rewriting, NAT traversal, registrar emulation — out of scope.
- Stateful/transaction proxying (retransmission absorption, forking) — stateless only.
- Answering any of these methods locally (e.g. local OPTIONS 200) — forward instead; SIP is
  not the liveness probe (HTTP health is `[STORY-001-008]`).
- TLS/SRTP — out of scope (PRD §8).

### Acceptance Criteria

#### AC1: REGISTER is forwarded to the PBX
**Given** the sequencer in front of a PBX
**When** an endpoint sends a REGISTER to the sequencer
**Then** the request is forwarded to the PBX unmodified (beyond proxy Via/Max-Forwards), and
the PBX's response is returned to the endpoint.

#### AC2: OPTIONS is forwarded, not answered locally
**Given** the sequencer
**When** an endpoint sends an OPTIONS to the sequencer
**Then** the OPTIONS is forwarded to the PBX and the PBX's response is returned — the
sequencer does not answer it itself.

#### AC3: Presence/messaging methods are forwarded
**Given** the sequencer
**When** an endpoint sends SUBSCRIBE, NOTIFY, MESSAGE, or PUBLISH
**Then** each is forwarded to the PBX and the PBX's response is returned to the originator.

#### AC4: Unmanaged methods never enter the application chain
**Given** an application sequence is configured
**When** any unmanaged method (e.g. REGISTER, MESSAGE) passes through the sequencer
**Then** no application in the sequence receives it — only the PBX does.

#### AC5: Call methods are still B2BUA-handled
**Given** the pass-through is active
**When** an endpoint sends an INVITE
**Then** it is handled by the B2BUA call path (app chain → PBX), not stateless-proxied —
pass-through does not regress call handling.

#### AC6: Response routing
**Given** a forwarded unmanaged request
**When** the PBX responds (including provisional then final)
**Then** the response is routed back to the original sender correctly (by Via), with no
hang and no duplicate.

#### Non-Functional Expectations
- Forwarding must add negligible latency and must not hold per-request state beyond the
  stateless proxy minimum.
- A malformed or unroutable unmanaged request must fail cleanly (appropriate SIP error to
  the sender) without affecting calls.