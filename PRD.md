# PRD — voipstack-sip-sequencer
**Status:** Approved (pivot to application sequencing — supersedes prior "RTP relay" PRD)
**Date:** 2026-06-24 (rev: SIP transports UDP/TCP/TLS/WS/WSS, TLS profiles, and timeouts;
orig. 2026-06-06)
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
    on_failure: skip        # skip | abort  (default skip)
    media: tap              # tap | none    (default none)
    transport: tcp          # tcp | tls (default tcp; udp NOT supported for apps; tls needs tls_profile)
    timeout: 5s             # per-app setup deadline (default: leg_timeout, §6.1)
  - name: record
    uri: sip:recorder.internal:5060
    on_failure: skip
    media: tap
  - name: route-guard
    uri: sip:guard.internal:5060
    on_failure: abort
    media: none
```
- `name` — identifier for logs/metrics. **Required.**
- `uri` — SIP URI / next-hop of the external application server. **Required.**
- `on_failure` — per-application sequencing semantic (see §7). Default `skip`.
- `media` — whether the application receives the call's audio (see §5):
  - `tap` — the application receives a **fork** of the call's media (both directions),
    as a recvonly two-`m=audio` (stereo) session. For media-consuming apps (transcribe,
    record).
  - `none` *(default when omitted)* — the application is offered no media (audio
    `inactive`); no RTP is sent to it. For signaling-only apps (auth, route-guard).
- `transport` — SIP transport for this application's leg: `tcp` *(default)* | `tls`
  (see §4.1). **`udp` is not supported for application legs** — the `media: tap` offer
  routinely exceeds the UDP MTU guard. `tls` **requires** a `tls_profile`.
- `tls_profile` — name of an entry in the top-level `tls_profiles` map (§6); only valid
  when `transport: tls`. Setting it on a non-TLS app is a startup error.
- `timeout` — Go duration (e.g. `5s`) bounding this app's leg setup (dial + answer). On
  expiry the leg fails and `on_failure` applies. Default: the global `leg_timeout` (§6.1).
  Must be `> 0` if set.

**Signaling vs media are orthogonal.** *Every* application is inserted into the SIP
**signaling** chain in order (so it can accept/reject — §7). `media` only controls
whether that application also receives the call's **audio**. A gatekeeper sits in the
signaling chain with `media: none`; a transcriber sits in the signaling chain with
`media: tap`.

### How an application is invoked
For each application in order, the sequencer **originates an INVITE** to the app's `uri`
over the app's `transport`, carrying the call's SDP offer (a stereo recvonly fork for
`media: tap`, an `inactive` audio offer for `media: none`) plus the informational
`X-Sequencer-Call-Id` / `X-Sequencer-Leg-Id` headers (§5). The application sees an
**ordinary inbound SIP call** and is unaware of the chain.
- **Provisional responses** (`1xx`) from the app are relayed back toward the caller.
- **The app answering with a final `2xx` establishes its leg** and is what "completes"
  the application for sequencing — the sequencer then advances to the **next** application.
  The leg is **not** torn down: it stays established for the call's lifetime so a
  `media: tap` app keeps receiving the fork.
- **Failure** (unreachable, non-2xx final, or `timeout` expiry) is handled per the app's
  `on_failure` (§7): `skip` logs/metrics and advances; `abort` fails the call.
After the last application, the sequencer originates the terminating leg to `next_hop`.

### 4.1 Transports (UDP / TCP / TLS / WS / WSS)
The sequencer speaks SIP over five transports. **Inbound listeners** and **outbound
legs** are configured independently.

- **Inbound listeners** (where endpoints/PBX reach the sequencer), configured in §6:
  - **`sip.listen`** binds **UDP and TCP together** on the same address/port — a UA or
    proxy is expected to offer both (RFC 3261 §18), and TCP also carries requests that
    exceed the UDP MTU guard. There is no separate key to enable TCP; it is always on.
  - **`tls.listen`** *(optional)* adds a TLS-over-TCP listener; it requires a `tls_profile`.
  - **`ws.listen`** *(optional)* adds a plain WebSocket listener (e.g. for a browser/jsSIP
    webphone). Plain transport; no profile.
  - **`wss.listen`** *(optional)* adds a secure WebSocket listener; like `tls.listen` it
    requires a `tls_profile`.
- **Outbound legs** (sequencer → application / next-hop) pick their transport from the
  per-endpoint `transport` field:
  - **Application legs are always TCP or TLS — `udp` is not supported.** The default is
    `tcp`; `transport: tls` upgrades to TLS (the app must accept TCP/TLS on its port). UDP
    is excluded because the `media: tap` offer (two `m=audio` lines) routinely exceeds the
    UDP MTU guard, so a UDP app leg would be refused at send.
  - **`next_hop`** honours its `transport` directly — `udp` *(default)* | `tcp` | `tls`.
    UDP **is** supported here (a small INVITE to the PBX fits the guard).
- **UDP path-MTU limit.** On UDP the sequencer enforces the RFC 3261 §18.1.1 guard:
  a request larger than ~1300 bytes is **refused at send** (no automatic TCP fallback).
  A routine INVITE with verbose headers can hit this — set `transport: tcp` on any
  `next_hop`/app that may carry large requests.
- **TLS** endpoints draw their certificate + verification policy from a named
  `tls_profiles` entry (§6); peer verification is **off by default** (encrypt-only),
  opt in with `verify_peer: true`.
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
  listen: 0.0.0.0:5060          # SIP listener — binds UDP *and* TCP on this port (§4.1)
next_hop:                       # terminating next-hop (PBX) — object form
  uri: pbx.internal:5060        #   required
  transport: udp                #   udp | tcp | tls (default udp; tls needs tls_profile)
  # tls_profile: outbound       #   only when transport: tls
rtp:
  port_range: 10000-20000       # anchored media port range (§5)
  idle_timeout: 5m              # tear down a call idle of all RTP/RTCP this long
                                #   (default 5m; 0 disables) — §6.1
media:
  public_address: 203.0.113.10  # optional: host advertised in ICE-lite candidates
                                #   (secured/WebRTC leg); defaults to the sip.listen host
log_level: info                 # debug | info | warn | error (default info)
leg_timeout: 32s                # global default outbound-leg setup/answer timeout (§6.1)
observability:
  listen: 0.0.0.0:9090          # Prometheus /metrics + /health (omit to disable) — §9
tls:                            # optional TLS-over-TCP listener (coexists with sip.listen)
  listen: 0.0.0.0:5061
  tls_profile: inbound
ws:                             # optional plain WebSocket listener (e.g. jsSIP webphone)
  listen: 0.0.0.0:8080
wss:                            # optional secure WebSocket listener
  listen: 0.0.0.0:8443
  tls_profile: inbound
tls_profiles:                   # named, reusable cert + crypto/verification policy (§4.1)
  inbound:
    cert: /etc/certs/server.pem
    key: /etc/certs/server.key
  outbound:
    cert: /etc/certs/client.pem
    key: /etc/certs/client.key
    ca: /etc/certs/ca.pem
    min_version: tlsv1.3        # tlsv1.2 (default) | tlsv1.3
    verify_peer: true           # default false (encrypt-only / no client cert)
    verify_depth: 3             # default 2
    verify_dates: true          # default true
    verify_subjects: [pbx.internal]   # optional allowed-subject list
    connect_timeout: 5s         # outbound TLS dial deadline (default 0 = unlimited)
sequence:                       # ordered application chain (§4)
  - name: transcribe
    uri: sip:transcriber.internal:5060
    on_failure: skip
    media: tap                  # tap | none (default none) — §4/§5
    transport: tcp              # tcp | tls (default tcp; udp not supported for apps) — §4.1
    timeout: 5s                 # per-app setup deadline (default leg_timeout) — §6.1
  - name: route-guard
    uri: sip:guard.internal:5060
    on_failure: abort
    media: none
```
**Required keys:** `sip.listen`, `next_hop.uri`, `rtp.port_range`, `sequence`. Everything
else is optional with documented defaults. Startup **fails fast** with a clear error on a
missing required key, an **unknown key** (the YAML is decoded with known-fields strict),
an invalid enum (`on_failure`/`media`/`transport`/`log_level`/`min_version`), an
unparseable duration, or **broken TLS wiring** — a `transport: tls` endpoint with no
`tls_profile`, a `tls_profile` on a non-TLS endpoint, or a reference to a profile that is
not defined. Certificate files are **not** opened at parse time (loadability surfaces at
startup when the listener/dialer is built). URI syntax, port-range bounds, and duplicate
`name`s are still not deeply validated and surface at use.

> **Breaking change vs the original v1 shape:** `next_hop` is now an **object**
> (`uri` + optional `transport`/`tls_profile`). The previous scalar form
> (`next_hop: host:port`) is no longer accepted.

### 6.1 Timeouts
Every long-running outbound operation is time-bounded; all values are Go duration strings.

- **`leg_timeout`** *(global, default `32s` — the SIP INVITE Timer B, `64·T1`)* — the
  default setup/answer deadline for **any** outbound leg: applications without their own
  `timeout`, the `next_hop`, and mid-call re-INVITE/REFER legs. Must be `> 0` if set.
- **`sequence[].timeout`** *(per-app, default = `leg_timeout`)* — overrides `leg_timeout`
  for one application's leg. The bound covers **both** the dial and the answer wait; the
  dial bound is what fast-fails a fully silent peer (a SIP `CANCEL` can only follow a
  provisional, so the answer wait alone cannot cut a silent peer short of Timer B). On
  expiry the app's `on_failure` decides skip vs abort (§7).
- **`rtp.idle_timeout`** *(default `5m`; `0` disables)* — an established call that
  exchanges **no RTP or RTCP in either direction** for longer than this is torn down,
  reclaiming its ports and relay goroutines when an endpoint or PBX vanishes **without a
  BYE**.
- **`tls_profiles[].connect_timeout`** *(per-profile, default `0` = unlimited)* — bounds
  the outbound **TLS dial** for legs using that profile.
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
- **SRTP for ordinary RTP peers** — media to/from standard SIP endpoints, applications,
  and the PBX is relayed as **plain RTP**. (SIP **signaling** transport security *is*
  supported — UDP/TCP/TLS/WS/WSS, §4.1 — only the RTP media path is unencrypted here.)
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
- **Transports — UDP/TCP/TLS/WS/WSS.** `sip.listen` binds UDP+TCP together; `tls.listen`,
  `ws.listen`, and `wss.listen` are optional additive listeners. Outbound legs select
  `transport` per endpoint (`udp`/`tcp`/`tls`); application legs always run over TCP/TLS
  (the tap offer exceeds the UDP MTU guard). TLS endpoints reference a named
  `tls_profiles` entry; peer verification is off by default (encrypt-only). (§4.1/§6)
- **Timeouts — bounded everywhere.** Global `leg_timeout` (default `32s`, SIP Timer B) with
  a per-app `timeout` override bounds outbound-leg setup; `rtp.idle_timeout` (default `5m`)
  reaps silent established calls; `connect_timeout` per TLS profile bounds the TLS dial. (§6.1)
- **Config validation — strict, fail-fast.** Unknown keys, invalid enums, unparseable
  durations, and broken TLS-profile wiring all abort startup with a clear error; cert files
  load lazily at listener/dialer build. (§6)
