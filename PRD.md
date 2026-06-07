# PRD — voipstack-sip-sequencer
**Status:** Approved (pivot to application sequencing — supersedes prior "RTP relay" PRD)
**Date:** 2026-06-06
**Owner:** bit4bit
**License:** GPLv3
## 1. Summary
`voipstack-sip-sequencer` is a **SIP application sequencer**: a back-to-back user agent
(B2BUA) deployed as a single binary that sits inline between SIP endpoints and a PBX.
For each call it routes the SIP dialog through an **ordered sequence of independent
external SIP application servers**, then on to the terminating destination.
The model follows **IMS-style application sequencing** (cf. S-CSCF *initial Filter
Criteria* and the JSR-289 *Application Router*), simplified: the order is a **static
list defined in YAML**, not a dynamic per-call decision and not backed by HSS/iFC XML.
The sequencer owns sequencing. Each application is an **independent SIP entity** — it
receives an ordinary SIP call, does its job, and is unaware it sits in a chain. It
does not know about the other applications and does not route to the next one; the
sequencer inserts each application in turn.
**Transcription is one application among many.** The earlier audio-fork/WebSocket
transcriber is no longer the product core — it becomes a reference external
application that the sequencer can chain to. The core product is the sequencer.
## 2. Goals
### Business objectives
- Compose multiple independent SIP services on a call without coupling them together.
- Add/remove/reorder call-processing applications by editing configuration only.
### Project objectives
- Easily deployable sequencer that chains a call through configured SIP applications.
- Production-ready performance.
### Technical objectives
- Written in **Go**.
- Built on **[emiago/sipgo](https://github.com/emiago/sipgo)**.
- Runs as a **B2BUA** on a single host.
- **Static YAML** application sequence. Each application is an independent block.
- Single binary + systemd deployment.
- Minimal configuration. Simple, repeatable release mechanism.
## 3. Sequencing model
```
                        ┌─────────────── application sequence (YAML order) ───────────────┐
                        │                                                                  │
 (SIP endpoint) ─INVITE─► voipstack-sip-sequencer ─► App #1 ─► back ─► App #2 ─► back ─► … ─► PBX
                  (B2BUA)        ▲   │            (ext SIP AS)        (ext SIP AS)
                                 │   │
                                 │   └─ for each app in order: bridge a B2BUA leg to the
                                 │      app's SIP URI; app processes; on completion the
                                 │      sequencer advances to the next app.
                                 └──────── sequencer owns the order; apps are independent.
```
- **Engine = B2BUA.** It terminates the incoming dialog and originates a new leg per
  application, maintaining the mapping between the incoming dialog and the chain legs.
- **Ordered chain.** For each application in the YAML list, in order, the sequencer
  bridges the call to that application's SIP URI. When the application completes its
  leg, the sequencer advances to the next. After the last application, it routes to the
  **terminating next-hop** (PBX).
- **Applications are independent.** An application receives a normal SIP call. It may
  act as a proxy, a B2BUA, or a UAS. It does **not** know its position in the chain and
  does **not** forward to the next application. The sequencer is the only component
  that knows and enforces the order.
- **Static, not dynamic (v1).** The sequence is fixed by configuration. No per-call
  Application Router logic, no iFC evaluation in v1.
## 4. Application definition (YAML)
The sequence is a list. **List order is the chain order.** Each entry is independent:
```yaml
sequence:
  - name: transcribe
    uri: sip:transcriber.internal:5060
    on_failure: skip        # skip | abort
    media: tap              # tap | none
  - name: record
    uri: sip:recorder.internal:5060
    on_failure: skip
    media: tap
  - name: route-guard
    uri: sip:guard.internal:5060
    on_failure: abort
    media: none
```
- `name` — identifier for logs/metrics.
- `uri` — SIP URI / next-hop of the external application server.
- `on_failure` — per-application sequencing semantic (see §7).
- `media` — whether the application receives the call's audio (see §5):
  - `tap` — the application receives a **fork** of the call's media (both directions),
    as a recvonly two-`m=audio` (stereo) session. For media-consuming apps (transcribe,
    record).
  - `none` *(default when omitted)* — the application is offered no media (audio
    `inactive`); no RTP is sent to it. For signaling-only apps (auth, route-guard).

**Signaling vs media are orthogonal.** *Every* application is inserted into the SIP
**signaling** chain in order (so it can accept/reject — §7). `media` only controls
whether that application also receives the call's **audio**. A gatekeeper sits in the
signaling chain with `media: none`; a transcriber sits in the signaling chain with
`media: tap`.
## 5. B2BUA behavior
- **Stateful per dialog.** The sequencer maintains the incoming dialog and the chain
  legs, and the mapping between them, for the call lifetime.
- **Correlation ids.** The sequencer mints a `call_id` (UUID, stable across the whole
  chain for one call) and a per-leg `leg_id` (UUID). They are carried on each outbound
  leg INVITE as **informational** headers `X-Sequencer-Call-Id` and
  `X-Sequencer-Leg-Id`. The SIP `Call-ID`/tags remain the sequencer's internal mapping
  input; applications may key on the `X-Sequencer-*` ids for a stable cross-leg handle.
- **Media — anchored, with per-app fork.** The sequencer **anchors RTP**: it owns the
  RTP ports and rewrites the `c=`/`m=` lines of every SDP it forwards so that **all media
  flows to the sequencer**, never directly between endpoint, applications, and PBX. This
  gives a deterministic media path.
  - **The call** is the anchored, bidirectional `endpoint ↔ sequencer ↔ PBX` audio path.
    The sequencer relays the two RTP streams (caller audio and callee audio) between the
    endpoint leg and the PBX leg.
  - **Application fork (`media: tap`).** For a tapping application, the sequencer offers
    its leg a **recvonly, two-`m=audio` (stereo) session** and **copies** each call
    direction into one `m=` line (caller audio → stream 1, callee audio → stream 2). The
    application thus receives **both directions of the same call** over its single SIP
    leg, as separate streams — "both legs arrive as one stereo session." The application
    only listens; it sends no audio back and is not in the call's audio path.
  - **No transcoding — ever.** The sequencer performs **no codec conversion, no mixing,
    no resampling, no audio processing of any kind**. It only **copies RTP packets**
    between sockets (relay) and **duplicates** them to tap legs (fork). The fork is a
    byte-for-byte copy split across two `m=` lines, not a mixed stereo stream. Whatever
    codec the parties negotiate is what every leg — including tap legs — carries,
    unchanged.
  - **Applications never inject or modify audio (v1).** Tap legs are recvonly observers.
    Apps that would play/insert/alter audio (announcements, IVR, transcoding) are out of
    scope (§8) — they would require mixing, which the sequencer does not do.
  - **Signaling-only apps (`media: none`)** are offered audio `inactive`; the sequencer
    sends them no RTP and allocates no fork for them.
- **Mid-call changes.** re-INVITE / hold propagate the new SDP through the **existing**
  chain legs — the chain is **not** re-run mid-call. REFER is handled **at the edge**:
  transfer re-points the endpoint leg; chain legs stay. Re-issuing a stable `call_id`
  across a transfer (new SIP Call-ID) is out of scope (§8).
- **Unmanaged-method pass-through.** The sequencer sits **in front of a PBX**; the PBX
  remains the registrar / feature server. Only the **call** methods are managed by the
  B2BUA (`INVITE`/`ACK`/`CANCEL`/`BYE`, plus mid-call `re-INVITE`/`UPDATE`/`PRACK`/`REFER`).
  Every **other** SIP method (`REGISTER`, `OPTIONS`, `MESSAGE`, `SUBSCRIBE`, `NOTIFY`,
  `PUBLISH`, `INFO`, …) is **transparently proxied to the terminating next-hop** (PBX),
  unmodified, so registration, presence, messaging, and keepalives keep working. These
  methods **never enter the application chain** — apps see calls only.
  - **v1 is a stateless forward** to the next-hop: no Record-Route, no Contact/registration
    rewriting. This covers REGISTER/OPTIONS/MESSAGE and simple flows; keeping the sequencer
    in the path for subscribe-dialog follow-ups behind NAT is **out of scope v1** (§8).
## 6. Configuration
**Single central YAML file is the sole source of configuration.** Its path is given on
startup as one CLI flag (e.g. `--config /etc/voipstack-sip-sequencer/config.yaml`).
- **No environment variables.** The process reads no env vars for behavior; everything
  is in the config file. This keeps configuration explicit, versionable, and
  reproducible — one file fully describes an instance.
- **No other config sources** — no flags-as-config beyond `--config`, no implicit
  defaults files, no remote config.
- The file is read **once at startup**. Reload behavior (SIGHUP / live reload) is out of
  scope for v1 (§8); a config change means a restart.
Full file shape:
```yaml
# config.yaml — complete instance configuration
sip:
  listen: 0.0.0.0:5060          # SIP listen address/port
next_hop: pbx.internal:5060     # terminating next-hop (PBX)
rtp:
  port_range: 10000-20000       # anchored media port range (§5)
sequence:                       # ordered application chain (§4)
  - name: transcribe
    uri: sip:transcriber.internal:5060
    on_failure: skip
    media: tap                  # tap | none (default none) — §4/§5
  - name: route-guard
    uri: sip:guard.internal:5060
    on_failure: abort
    media: none
```
Required keys: `sip.listen`, `next_hop`, `rtp.port_range`, `sequence`. Startup fails
fast with a clear error if a required key is missing. No deep validation (URI syntax,
port-range bounds, duplicate `name`) in v1 — malformed values surface at use.
## 7. Sequencing semantics & failure handling
- **Linear chain (v1).** No branching, no loops; applications run once each, in order.
- **Per-application failure policy** via `on_failure`:
  - `abort` — application is required; if it is unreachable or rejects, the call fails.
  - `skip` — application is best-effort; on failure the sequencer logs, emits a metric,
    and advances to the next application. The call is not dropped.
  - **Default when omitted: `skip`** — sequencing is additive, so a dead optional app
    must not kill calls. Gatekeeper apps (auth/route-guard) set `abort` explicitly.
- An application "completing its leg" advances the chain; explicit signaling of "next"
  by applications is out of scope (apps stay unaware of the chain).
## 8. Non-goals (out of scope, v1)
- **Transcription / audio analysis** — now performed by an external application, not
  the sequencer.
- **Audio processing / transcoding of any kind.** The sequencer never converts codecs,
  resamples, mixes, or alters audio. It only copies/relays RTP packets and duplicates
  them to tap legs (§5). No DTMF detection, no media gateway behavior.
- **Audio injection / modification by applications** — tap legs are recvonly observers;
  apps that would play, insert, or alter call audio (announcements, IVR) need mixing,
  which the sequencer does not do.
- **Dynamic application routing** — no per-call Application Router; static YAML only.
- **IMS infrastructure** — no HSS/Sh, no iFC XML, no ISC profiles. The sequencing
  *model* is borrowed; the *machinery* is a static list.
- **Branching / looping / conditional sub-chains** — linear sequence only.
- TLS for SIP / SRTP for media (plain SIP + RTP only in v1).
- Multi-tenant — single PBX / single sequence per instance.
- Persistence / storage. UI / dashboard.
- **Environment-variable configuration** — config lives only in the central YAML file
  (§6); no env vars drive behavior.
- **Live config reload** — config is read once at startup; changes require a restart.
## 9. Performance & observability
- Target: **100 concurrent calls** on a single host.
- **Prometheus** metrics. Initial set: active calls, active dialogs/legs, per-application
  invocation count, per-application failures, sequencing latency, terminating-hop
  failures.
- Health endpoint for liveness.
## 10. Deployment & release
- **Single static Go binary.**
- **systemd** unit for service management.
- Simple, repeatable release mechanism (tagged builds producing the binary).
## 11. Decisions (resolved)
- **Media — anchored, per-app fork, no transcoding.** Sequencer owns RTP ports and
  rewrites SDP so all media flows through it. The call is the anchored
  `endpoint ↔ sequencer ↔ PBX` path. Apps with `media: tap` receive a **fork** of both
  call directions as a recvonly two-`m=audio` (stereo) session — a byte-for-byte copy,
  not a mix. The sequencer does **no transcoding/mixing/resampling** of any kind; it only
  copies RTP. Apps with `media: none` get no media. Apps never inject audio (v1). (§4/§5)
- **Signaling vs media orthogonal.** Every app is in the signaling chain (can
  accept/reject, §7) regardless of `media`; `media` only controls whether it also
  receives the call's audio. (§4)
- **Correlation handoff — headers.** `X-Sequencer-Call-Id` / `X-Sequencer-Leg-Id` on
  each outbound leg INVITE, informational. SIP `Call-ID`/tags stay internal. (§5)
- **Failure default — `skip`.** Omitted `on_failure` is best-effort; `abort` is
  explicit for required apps. (§7)
- **Mid-call — update legs, no re-run.** re-INVITE/hold propagate new SDP through the
  existing chain; the chain is not re-sequenced mid-call. (§5)
- **REFER — edge only.** Transfer re-points the endpoint leg; chain legs stay; no
  cross-Call-ID `call_id` reissue (out of scope, §8). (§5)
- **Config — single central YAML, no env vars.** One `--config` file is the sole
  configuration source; read once at startup; no environment variables. (§6)
