# sip-sequencing

`voipstack-sip-sequencer` — a **SIP application sequencer**. A back-to-back user agent
(B2BUA) deployed as a single binary that sits inline between SIP endpoints and a PBX.
For each call it routes the SIP dialog through an **ordered sequence of independent
external SIP application servers**, then on to the terminating destination.

Each application is an **independent SIP entity** — it receives an ordinary SIP call,
does its job, and is unaware it sits in a chain. The sequencer owns the order and
inserts each application in turn.

See [PRD.md](PRD.md) for the full product definition.

## ⚠️ Status: Under Construction

This project is **under active development** and should be used **at your own risk**. APIs, configuration, and behavior may change without notice. Do not use in production environments without thorough testing and validation.

## How it works

```
                        ┌─────────── application sequence (YAML order) ───────────┐
                        │                                                          │
 (SIP endpoint) ─INVITE─► sip-sequencer ─► App #1 ─► back ─► App #2 ─► back ─► … ─► PBX
                  (B2BUA)      ▲   │      (ext SIP AS)      (ext SIP AS)
                              media anchored: all RTP flows through the sequencer
```

- **Engine = B2BUA.** Terminates the incoming dialog, originates a new leg per
  application, and maintains the mapping for the call lifetime.
- **Ordered chain.** Each application in the YAML list is bridged in order. After the
  last, the call routes to the terminating next-hop (PBX).
- **Media anchored, no transcoding.** The sequencer owns RTP ports and rewrites SDP so
  all media flows through it. Apps with `media: tap` receive a byte-for-byte **fork** of
  both call directions as a recvonly two-`m=audio` (stereo) session. No codec
  conversion, mixing, or resampling — ever.
- **Pass-through.** Only call methods (`INVITE`/`ACK`/`CANCEL`/`BYE` + mid-call
  `re-INVITE`/`UPDATE`/`PRACK`/`REFER`) are managed. Every other method (`REGISTER`,
  `OPTIONS`, `MESSAGE`, `SUBSCRIBE`, …) is transparently proxied to the PBX.

## Build

Requires **Go 1.23.6+**.

```sh
# build a static binary via the Makefile (CGO_ENABLED=0 -trimpath, version-stamped)
make build                       # -> dist/sip-sequencer

# tag-named release artifact + sha256 checksum
make release VERSION=v1.2.3      # -> dist/sip-sequencer-v1.2.3-linux-amd64(.sha256)

# or build directly
CGO_ENABLED=0 go build -o sip-sequencer ./cmd/sip-sequencer

# or install into $GOBIN
go install github.com/voipstack/voipstack-sip-sequencer/cmd/sip-sequencer@latest

# run tests
make test                        # go test -race ./...
```

See [packaging/README.md](packaging/README.md) for install, systemd, and upgrade instructions.

## Run

The single `--config` YAML file is the sole source of configuration. No environment
variables. The file is read once at startup; a config change means a restart.

```sh
./sip-sequencer --config /etc/voipstack-sip-sequencer/config.yaml
```

## Configuration

Complete instance configuration. Required keys: `sip.listen`, `next_hop` (an object with
`uri`), `rtp.port_range`, `sequence`. Startup fails fast with a clear error if a required
key is missing or a reference is broken.

```yaml
# config.yaml — complete instance configuration
sip:
  listen: 0.0.0.0:5060          # SIP listen address/port
next_hop:                       # terminating next-hop (PBX) — object form
  uri: pbx.internal:5060
  transport: udp                # udp | tcp | tls (default udp; tls needs tls_profile)
rtp:
  port_range: 10000-20000       # anchored media port range
  idle_timeout: 5m              # tear down a call idle of all RTP/RTCP this long (default 5m; 0 disables)
observability:
  listen: 0.0.0.0:9090          # Prometheus /metrics + /health (omit to disable)
log_level: info                 # debug | info | warn | error (default info)
leg_timeout: 32s                # global default leg setup/answer timeout (default 32s)
sequence:                       # ordered application chain — list order IS chain order
  - name: transcribe
    uri: sip:transcriber.internal:5060
    on_failure: skip            # skip | abort
    media: tap                  # tap | none (default none)
    transport: tcp              # udp | tcp | tls (default udp; tls needs tls_profile)
    timeout: 5s                 # per-app setup deadline (default: leg_timeout)
  - name: record
    uri: sip:recorder.internal:5060
    on_failure: skip
    media: tap
  - name: route-guard
    uri: sip:guard.internal:5060
    on_failure: abort
    media: none
```

> **Breaking change:** `next_hop` is now an **object** (`uri` + optional
> `transport`/`tls_profile`). The previous scalar form (`next_hop: host:port`) is no
> longer accepted; migrate to `next_hop:\n  uri: host:port`.

### Application fields

| Field        | Meaning                                                                 |
|--------------|-------------------------------------------------------------------------|
| `name`       | Identifier for logs/metrics.                                             |
| `uri`        | SIP URI / next-hop of the external application server.                   |
| `on_failure` | `abort` — required app; failure fails the call. `skip` — best-effort; on failure log, emit metric, advance. **Default: `skip`.** |
| `media`      | `tap` — app receives a fork of the call audio (stereo, recvonly). `none` *(default)* — no media (audio `inactive`). |
| `transport`  | `udp` *(default)* \| `tcp` \| `tls`. `tls` requires a `tls_profile` naming an entry in `tls_profiles`. |
| `timeout`    | Go duration (e.g. `5s`) bounding this app's leg setup (dial + answer). On expiry the leg fails and `on_failure` applies. **Default: `leg_timeout`.** Must be `> 0` if set. |

**Signaling vs media are orthogonal.** Every app is inserted into the SIP signaling
chain in order (so it can accept/reject) regardless of `media`; `media` only controls
whether it also receives the call's audio.

### Next-hop fields

| Field                | Meaning                                                                 |
|----------------------|-------------------------------------------------------------------------|
| `next_hop.uri`       | Required. SIP URI / `host:port` of the terminating hop (PBX).            |
| `next_hop.transport` | `udp` *(default)* \| `tcp` \| `tls`. `tls` requires a `tls_profile`.     |
| `next_hop.tls_profile` | Name of a `tls_profiles` entry. Only valid when `transport: tls`.     |

### Timeouts

| Field         | Meaning                                                                 |
|---------------|-------------------------------------------------------------------------|
| `leg_timeout` | Go duration (e.g. `32s`) — global default setup/answer timeout for any outbound leg: applications without their own `timeout`, the `next_hop`, mid-call re-INVITEs, and REFER. **Default `32s`** (the SIP INVITE Timer B). Must be `> 0` if set. |

A per-app `timeout` overrides `leg_timeout` for that application's leg only. The bound covers
both the dial and the answer wait; the dial bound is what fast-fails an unreachable app
(SIP `CANCEL` can only follow a provisional, so the answer wait alone cannot cut a fully
silent peer short of Timer B). On expiry the app's `on_failure` decides skip vs abort.

### TLS configuration

> **Parse/validate only (US12).** TLS config is **parsed, resolved, and validated** at
> startup, but TLS listeners and handshakes are **not yet wired** (US13–16). A `transport:
> tls` endpoint or a `tls.listen` block is accepted and checked for correct wiring, but
> does not yet establish TLS. Use plain `udp`/`tcp` until US13–16 land.

Named, reusable `tls_profiles` (certificate material + crypto/verification/timeout
policy) are referenced by name from TLS endpoints — the optional `tls.listen` listener,
`sequence` items, and `next_hop`. Defaults: TLS 1.2 floor, verify depth 2, dates checked.
**Peer verification is off by default** — an inbound listener requires no client
certificate, and an **outbound leg is encrypt-only: it accepts any server certificate**
(self-signed, expired, hostname mismatch, untrusted CA). Set `verify_peer: true` on a
profile to validate the peer: mutual TLS on an inbound listener, or full chain + dates +
hostname verification on an outbound leg.

```yaml
tls:                            # optional TLS listener (coexists with sip.listen)
  listen: 0.0.0.0:5061
  tls_profile: inbound
tls_profiles:
  inbound:
    cert: /etc/certs/server.pem
    key: /etc/certs/server.key
  outbound:
    cert: /etc/certs/client.pem
    key: /etc/certs/client.key
    ca: /etc/certs/ca.pem
    min_version: tlsv1.3        # tlsv1.2 (default) | tlsv1.3
    verify_peer: true           # default false
    verify_depth: 3             # default 2
    verify_dates: true          # default true
    verify_subjects:            # optional allowed-subject list
      - pbx.internal
    connect_timeout: 5s         # Go duration; default 0 (unlimited)
```

| Field             | Meaning                                                            |
|-------------------|--------------------------------------------------------------------|
| `cert` / `key`    | Required. Certificate and private key paths.                       |
| `passphrase`      | Optional private-key passphrase.                                   |
| `ca`              | Optional CA bundle for peer verification.                          |
| `min_version`     | `tlsv1.2` *(default)* \| `tlsv1.3`.                                 |
| `ciphers`         | Optional cipher list (validated by the TLS provider, not here).    |
| `verify_peer`     | Validate the peer certificate. **Default `false`** — inbound requires no client cert; outbound is encrypt-only (any server cert accepted). `true` enforces mTLS (inbound) / chain + dates + hostname (outbound). |
| `verify_depth`    | Max chain depth. **Default `2`.**                                  |
| `verify_dates`    | Enforce cert validity dates. **Default `true`.**                   |
| `verify_subjects` | Optional list of allowed certificate subjects.                     |
| `connect_timeout` | Go duration (e.g. `5s`). **Default `0`** (unlimited).              |

Profiles are validated for wiring (every referenced name must exist; a `tls_profile` on a
non-TLS endpoint is rejected) but **no certificate files are opened at parse time** —
loadability is checked downstream (US13).

### Observability fields

| Field                  | Meaning                                                         |
|------------------------|-----------------------------------------------------------------|
| `observability.listen` | `host:port` for the HTTP observability server. Omit or leave empty to disable. |

**Endpoints** (when `observability.listen` is set):

| Path       | Method | Response                                         |
|------------|--------|--------------------------------------------------|
| `/metrics` | GET    | Prometheus text exposition (see metrics below).  |
| `/health`  | GET    | `200 ok` — process liveness.                     |

**Prometheus metrics:**

| Metric                                       | Type      | Labels | Description                              |
|----------------------------------------------|-----------|--------|------------------------------------------|
| `sequencer_active_calls`                     | Gauge     | —      | Calls currently active in the registry.  |
| `sequencer_active_legs`                      | Gauge     | —      | Legs active across all calls.            |
| `sequencer_app_invocations_total`            | Counter   | `app`  | Successful app-leg completions.          |
| `sequencer_app_failures_total`               | Counter   | `app`  | App-leg failures.                        |
| `sequencer_terminating_hop_failures_total`   | Counter   | —      | Failed terminating-hop (PBX) attempts.   |
| `sequencer_sequencing_duration_seconds`      | Histogram | —      | Per-call setup latency.                  |

```sh
# scrape
curl http://localhost:9090/metrics

# liveness probe
curl http://localhost:9090/health
```

## Status

Tracked against the [user stories](requirements/).

**Implemented**

- Config loading — single `--config` YAML, fail-fast on missing keys (US1)
- B2BUA single-app bridge — inline, completes a call through one app (US2)
- Ordered multi-app chain — sequence in YAML order, then PBX (US3)
- Per-app failure handling — `abort` / `skip` (US4)
- RTP media anchoring — sequencer owns ports, rewrites SDP (US5)
- Correlation ids — `X-Sequencer-Call-Id` / `X-Sequencer-Leg-Id` (US6)
- Media fork to apps — `media: tap` stereo recvonly (US10)
- Unmanaged-method pass-through — stateless forward to PBX (US11)
- Observability — Prometheus `/metrics` + `/health` endpoint, opt-in via `observability.listen` (US8)
- Deployment / release — single static binary, systemd unit, tagged release builds (US9)
- TLS config model — parse/resolve/validate `tls_profiles` + `transport` + `tls.listen` wiring (US12)

**Not yet implemented**

- Mid-call signaling — re-INVITE / hold / REFER propagation (US7)
- TLS runtime — certificate loading, TLS listeners, outbound TLS dialing/handshake (US13–16)

## Use Cases

The sequencer decouples call-processing services, letting you compose them independently:

- **Compliance: Call Recording + Transcription.** Route calls through a recorder, then a
  transcriber. Both tap the media; neither knows the other exists. Add/remove either
  without touching your PBX.
- **Call Validation & Authorization.** Chain a gatekeeper app (auth, route-guard, fraud
  check) before the main call logic. Reject invalid calls early; forward valid ones to
  the PBX.
- **Multi-stage Processing.** Combine independent services in order: validate caller →
  record call → transcribe → route. Each app is a black box; the sequencer owns the
  chain.
- **Feature Roll-out Without Coupling.** Add a new call-processing feature by inserting
  a new app in the YAML config. Existing apps stay unchanged. Reorder apps or toggle
  them on/off by editing config alone — no code changes, no redeploy of other services.
- **Selective Feature Tapping.** Some apps tap audio (media: tap); others are signaling
  only (media: none). A transcriber listens; a gatekeeper blocks or allows. Both sit in
  the same chain without interfering.

## Deployment

Single static Go binary run under **systemd** on one host (target: 100 concurrent calls).
`make release` turns a tag into a deterministic, version-stamped artifact + `sha256`
checksum; `make deb` builds a Debian `.deb` (via [nfpm](https://nfpm.goreleaser.com)) that
installs the binary, unit, and an initial `/etc` config as a preserved conffile. Upgrades
are `dpkg -i` the new `.deb` (or replace-binary) + `systemctl restart` — no migration. The
unit runs non-root with only `CAP_NET_BIND_SERVICE` and surfaces a bad config as a `failed`
unit.

Artifacts: [`Makefile`](Makefile), [`packaging/systemd/voipstack-sip-sequencer.service`](packaging/systemd/voipstack-sip-sequencer.service),
[`packaging/config.example.yaml`](packaging/config.example.yaml),
[`packaging/nfpm.yaml`](packaging/nfpm.yaml). Full guide:
[packaging/README.md](packaging/README.md).

## Quick Start: Side-by-side with FreeSWITCH

Deploy on the **same host or container** as your PBX (e.g. FreeSWITCH) so you can add applications without changing any PBX configuration.

### Goal

- FreeSWITCH keeps listening on its usual profile port (e.g. `5060`).
- The sequencer listens on a new port on the **same IP** (e.g. `5080`).
- Endpoints send calls to the sequencer on `5080`.
- The sequencer chains applications, then forwards to FreeSWITCH on `5060`.

### 1. Install

Download the latest `.deb` from [GitHub Releases](https://github.com/voipstack/voipstack-sip-sequencer/releases) (filename: `voipstack-sip-sequencer_latest_amd64.deb`):

```sh
sudo dpkg -i voipstack-sip-sequencer_latest_amd64.deb
```

This installs the binary, systemd unit, and a sample config at `/etc/voipstack-sip-sequencer/config.yaml`. The config is a `dpkg` **conffile** — your edits survive upgrades.

### 2. Configure

Edit `/etc/voipstack-sip-sequencer/config.yaml`. Use the **same IP** as the FreeSWITCH profile you want to route to, but a **different port** for the sequencer and a **non-overlapping RTP range**:

```yaml
sip:
  listen: 192.168.1.10:5080      # same interface as FreeSWITCH, different port

next_hop:
  uri: 192.168.1.10:5060         # FreeSWITCH's internal profile (UDP)

rtp:
  port_range: 30000-30100        # must not overlap with FreeSWITCH's range

sequence:
  - name: transcriber
    uri: sip:transcriber.internal:5060
    on_failure: skip
    media: tap

observability:
  listen: 0.0.0.0:9090

log_level: info
```

### 3. Start

```sh
sudo systemctl start voipstack-sip-sequencer
```

### 4. Route your UAC to the sequencer

Point your endpoint/trunk SIP server from `192.168.1.10:5060` to `192.168.1.10:5080`.

Call flow becomes:

```
Endpoint ─► Sequencer (5080) ─► Transcriber ─► Sequencer ─► FreeSWITCH (5060)
```

### 5. Rollback

If you ever want to bypass the sequencer, point the UAC back to `:5060`. FreeSWITCH was never reconfigured, so it continues working exactly as before.

## Scope & Limitations (v1)

Current constraints and features not yet supported. See [PRD.md §8](PRD.md) for the full non-goals list.

- **Transport:** Inbound listener is UDP. Application legs always use TCP (so large SDP offers are not capped by the UDP MTU guard); the next-hop leg is UDP by default and can opt into TCP via `transport: tcp`. TLS is **config-only** so far — `transport: tls` and `tls.listen` parse and validate but do not yet establish TLS (runtime is US13–16). No WebSocket.
- **Media security:** Plain RTP only. No SIPS, SIP over TLS, or SRTP.
- **NAT traversal:** No STUN, TURN, ICE, or hole-punching for SIP or RTP.
- **Sequencing:** Static linear sequence only. No branching, looping, or dynamic routing.
- **Media processing:** No transcoding, mixing, or audio injection.
- **Topology:** Single PBX / single sequence per instance.

## License

GPLv3 — see [LICENSE](LICENSE).

---

> **Note:** This project's documentation and code were created with the assistance of
> a Large Language Model (LLM).
