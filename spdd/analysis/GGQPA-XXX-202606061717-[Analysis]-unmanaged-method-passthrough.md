# SPDD Analysis: Transparent pass-through of unmanaged SIP methods

> Phase 0 (analysis) for `[STORY-001-011]`. Strategic level. The "How" (sipgo proxy calls)
> is left to `/spdd-reasons-canvas`.

## Codebase grounding (working notes)
- **Stories 001/002 implemented; 003 in progress.** `internal/b2bua/engine.go` registers
  only call handlers: `srv.OnInvite(handleInvite)`, `srv.OnAck(...ReadAck)`,
  `srv.OnBye(...ReadBye)`. No handler exists for REGISTER/OPTIONS/MESSAGE/SUBSCRIBE/etc., so
  today they would be unhandled.
- `Engine` already holds `cli *sipgo.Client`, `srv *sipgo.Server`, `cfg` (so `cfg.NextHop`
  is available), and `runCtx`. A proxy handler fits here with no new wiring beyond handler
  registration.
- Transport is **UDP** (story 002). `cfg.NextHop` is the PBX address.
- `AGENTS.md`: functional core / edges, errors as values, real fakes for tests (no internal
  mocks), `-race` clean.

## Original Business Requirement

> Complete `[STORY-001-011]` text — see `requirements/[User-story-11]…md`. Summary: the
> sequencer is inline in front of a PBX; the PBX stays registrar/feature server; the B2BUA
> manages call methods only; **every other SIP method is transparently stateless-proxied to
> `next_hop`** and never enters the app chain. v1 = stateless forward, no Record-Route, no
> Contact rewriting.

(Accepted decisions: D1 managed = INVITE/ACK/CANCEL/BYE/+mid-call; bypass = all others.
D2 stateless proxy. D3 minimal forward, no Record-Route/Contact rewrite v1; OPTIONS
forwarded not answered locally.)

## Domain Concept Identification

#### Existing Concepts (from codebase)
- **`Engine`** (`engine.go`): owns the SIP server/client + next-hop. Gains a proxy handler.
- **Call handlers** (`OnInvite/OnAck/OnBye`): the "managed" set. Unchanged; pass-through is
  additive and must not touch them.
- **`cfg.NextHop`**: the PBX target — the single forward destination.

#### New Concepts Required
- **Method classification:** managed (call dialog) vs unmanaged (everything else). The
  routing predicate.
- **Stateless proxy forward:** receive an unmanaged request → build the forward to
  `next_hop` (add Via, decrement Max-Forwards) → send via client transaction → relay
  responses back to the inbound server transaction by Via.
- **Pass-through boundary:** unmanaged traffic bypasses the application chain entirely.

#### Key Business Rules
- **PBX owns non-call SIP:** every unmanaged method goes to `next_hop`, unmodified beyond
  proxy headers. AC1/AC2/AC3.
- **Apps are call-only:** unmanaged methods never enter the chain. AC4.
- **Calls unaffected:** INVITE/ACK/CANCEL/BYE still B2BUA-handled. AC5.
- **Correct response routing:** responses returned to the originator by Via, no hang/dup.
  AC6.
- **Transparent:** sequencer adds only what a stateless proxy must (Via/Max-Forwards); no
  Record-Route, no Contact rewrite (v1).

## Strategic Approach

#### Solution Direction
- Add a **stateless proxy handler** (`proxy.go`) in `internal/b2bua`, registered for the
  unmanaged methods on `srv` in `Run`. One handler, parameterized by nothing but
  `cfg.NextHop`.
- Forward: copy the inbound request, set the Request-URI / destination to `next_hop`, push a
  new `Via` (branch), decrement `Max-Forwards` (reject at 0), send with the existing
  `cli` as a client transaction; pump every PBX response (provisional + final) back to the
  inbound `ServerTransaction`, stripping the top Via as a proxy does.
- **Method routing:** register the handler for each unmanaged method sipgo exposes
  (`OnRegister`, `OnOptions`, `OnSubscribe`, `OnNotify`, `OnMessage`, `OnInfo`, `OnPublish`,
  …); if sipgo offers a catch-all/no-route hook, prefer that for completeness. Keep
  `OnInvite/OnAck/OnBye` as-is.
- **Tests:** real in-memory fakes — a fake endpoint (UAC) sending REGISTER/OPTIONS/MESSAGE,
  a fake PBX (UAS) that records what it received and replies; assert the PBX got the method,
  the originator got the PBX's status, and (with an app fake) the app got nothing.

#### Key Design Decisions
- **Stateless vs stateful proxy:** → **stateless** (accepted). Simpler; sufficient for
  REGISTER/OPTIONS/MESSAGE. Rationale: YAGNI; PBX owns retransmission semantics.
- **Per-method registration vs catch-all:** → prefer a **single handler** wired to all
  unmanaged methods (catch-all if sipgo exposes one; otherwise explicit per-method list).
  Rationale: one code path; explicit list is fine and readable.
- **OPTIONS:** → **forward**, do not answer locally. Rationale: stay transparent; HTTP
  health is the liveness probe (story 008).
- **Max-Forwards / loop safety:** → decrement and reject at 0 (`483 Too Many Hops`).
  Rationale: basic proxy hygiene.

#### Alternatives Considered
- **Answer OPTIONS locally / emulate registrar:** rejected — PBX owns these; transparency is
  the whole point.
- **Stateful transaction proxy:** rejected for v1 — unneeded complexity.
- **Record-Route to stay in subscribe dialogs:** rejected v1 — explicitly out of scope;
  separate later story if NAT/presence-dialog persistence is needed.

## Risk & Gap Analysis

#### Requirement Ambiguities
- **sipgo proxy support:** how much stateless-proxy plumbing sipgo gives (Via handling,
  response pumping) vs hand-rolled. Must confirm the available primitives in REASONS Canvas
  (e.g. `cli.TransactionRequest` + relaying responses to `tx`).
- **Catch-all availability:** whether sipgo has a generic/no-route handler; if not, the
  unmanaged method list must be enumerated explicitly.
- **PBX→endpoint direction:** requests the PBX originates (e.g. NOTIFY for MWI) only traverse
  the sequencer if the PBX routes them here — depends on registration/Contact, which v1 does
  not rewrite. Known limitation; forward whatever arrives.

#### Edge Cases
- **Max-Forwards = 0** → `483`. Malformed Request-URI / unroutable next-hop → `5xx` to
  sender, calls unaffected.
- **In-dialog unmanaged requests** (e.g. in-dialog NOTIFY for a SUBSCRIBE established without
  Record-Route) may not route back through the sequencer — v1 limitation, documented.
- **CANCEL/ACK** are call-related — must remain managed, not proxied (don't misclassify).
- **High volume of OPTIONS keepalives** — forwarding must be cheap, stateless.

#### Technical Risks
- **Misclassification regression:** accidentally proxying a call method would break calls.
  Mitigation: explicit managed set; AC5 regression test; keep `OnInvite/OnAck/OnBye`.
- **Response routing correctness:** Via handling must return responses to the right sender
  without duplicates/hangs. Mitigation: standard top-Via strip; fake-PBX test asserting the
  originator's received response.
- **Concurrency:** many concurrent stateless forwards; no shared mutable state needed → low
  risk; `-race`.

#### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | REGISTER forwarded to PBX | Yes | stateless forward to next_hop. |
| AC2 | OPTIONS forwarded, not local | Yes | no local answer. |
| AC3 | SUBSCRIBE/NOTIFY/MESSAGE/PUBLISH forwarded | Yes | same handler. |
| AC4 | Unmanaged never enters chain | Yes | handler bypasses bridge; assert app sees nothing. |
| AC5 | Call methods still B2BUA | Yes | managed set untouched; regression test. |
| AC6 | Response routing by Via | Yes | confirm sipgo response pumping. |
| NFR | Negligible latency / clean failures | Partial | achievable; verify forward path is stateless. |

**Net:** addressable as a stateless proxy handler reusing the existing `Engine` client +
`cfg.NextHop`, registered for unmanaged methods, bypassing the chain. Load-bearing items for
REASONS Canvas: (1) sipgo proxy primitives + catch-all availability, (2) exact managed set,
(3) Via/Max-Forwards handling, (4) documented v1 limitation (no Record-Route).
