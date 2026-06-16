# Sequencer test box (Vagrant + FreeSWITCH + Debian)

One Debian 12 VM that lets you exercise `voipstack-sip-sequencer` end to end with
real softphones and a browser jsSIP phone.

All native in the VM — no containers.

```
                                  VM 192.168.56.10
  softphone 1001/1002 ──5060/udp──► FreeSWITCH (PBX + registrar, native)
                                         ▲
  softphone 1003/1004 ──5070/udp──► sip-sequencer ──5060──┘   (REGISTER + Path,
  browser jsSIP 1005  ──ws 8080───►   (the proxy)  └─ recording tap (TCP :6060)
                                         │
                                    RTP 30000-30100
```

All extensions ultimately register at FreeSWITCH (users **1000–1019**, password
**voipstacklab**). Calls route through FreeSWITCH's default dialplan, so any extension can
call any other regardless of which path it registered through.

## Start / stop

FreeSWITCH installs natively from SignalWire's apt repo, which needs a **free
Personal Access Token** (register at signalwire.com → Personal Access Token).
Export it before provisioning:

```sh
cd testenv
export SIGNALWIRE_TOKEN=PT....            # your token; do not commit it
vagrant up          # first run is slow: Debian box, Go, FreeSWITCH packages
vagrant halt        # stop the VM (services restart on next `up`)
vagrant provision   # re-run provisioning after editing config/units
vagrant destroy -f  # full teardown
```

Works on libvirt/KVM or VirtualBox + Vagrant. The VM gets host-only IP
`192.168.56.10`; the webphone/WebSocket/metrics ports are forwarded to the
**Vagrant host's** `localhost`.

> libvirt note: `vagrant reload` can crash (`virDomainSetMemory ... domain is not
> running`). Use `vagrant halt && vagrant up` instead of `reload`.

Check it came up (run on the host, or `vagrant ssh` first):

```sh
vagrant ssh -c "systemctl is-active freeswitch sip-sequencer recording-app webphone"
curl http://localhost:9090/health     # -> 200 ok
curl http://localhost:9090/metrics    # sequencer Prometheus metrics
```

## 1 & 2 — Softphones (Linphone, Zoiper, MicroSIP, …)

Use **two** extensions direct to FreeSWITCH and **two** through the sequencer.
Domain/realm is `192.168.56.10` for all; password `voipstacklab`; transport **UDP**.

| Ext  | Path            | SIP server / proxy      | Note                     |
|------|-----------------|-------------------------|--------------------------|
| 1001 | direct to PBX   | `192.168.56.10:5060`    | baseline, bypasses proxy |
| 1002 | direct to PBX   | `192.168.56.10:5060`    | baseline                 |
| 1003 | **via proxy**   | `192.168.56.10:5070`    | REGISTER/INVITE via sequencer |
| 1004 | **via proxy**   | `192.168.56.10:5070`    | "                        |

In most softphones set: *username* = ext, *auth user* = ext, *password* = `voipstacklab`,
*domain* = `192.168.56.10`, *outbound proxy* = the server above (5060 or 5070).

## 3 — Web jsSIP phone

WebRTC needs a *secure context*: the page must be `https://` **or** served from
`localhost`. The forwarded ports make `http://localhost:8090` work — **but only in
a browser running on the Vagrant host itself**.

If your browser is on another machine, tunnel the forwarded ports to your
workstation so they appear on *its* localhost:

```sh
ssh -L 8090:127.0.0.1:8090 -L 8080:127.0.0.1:8080 user@vagrant-host
```

then open `http://localhost:8090`. Caveat: signaling tunnels, but WebRTC **media**
(RTP/ICE) targets `192.168.56.10:30000-30100` — reachable only from the Vagrant
host's network. For audio from a remote browser you must instead make the VM
reachable on a routable IP, set `media.public_address` to it, and serve the page
over HTTPS + `wss://`.

Open the page, then: defaults ext **1005**, WS `ws://localhost:8080`, domain
`192.168.56.10`.
Click **Register**, then call e.g. `1001`. Grant the mic permission.

## Test call matrix

| From → To     | Exercises                                   |
|---------------|---------------------------------------------|
| 1001 → 1002   | FreeSWITCH only (sanity, no proxy)          |
| 1001 → 1003   | direct → proxy-registered (Path round-trip) |
| 1003 → 1004   | proxy UDP B2BUA both legs + recording tap   |
| 1001 → 1005   | direct → jsSIP web (WS + WebRTC↔RTP anchor) |
| 1005 → 1003   | jsSIP web → proxy-registered                |

Each should ring and give two-way audio.

## Observe

```sh
vagrant ssh
journalctl -u sip-sequencer -f          # app-chain + register-forwarding logs
journalctl -u recording-app -f
ls -la /var/lib/recording-app/          # WAVs prove the media:tap fork fired
journalctl -u freeswitch -f             # FreeSWITCH service log
fs_cli                                  # FreeSWITCH console: `sofia status`, `sofia status profile internal reg`
```

## Troubleshooting (known risk points)

- **Inbound call to a proxy-registered ext (1003/1005) does not ring.** The
  sequencer inserts an RFC 3327 `Path` on forwarded REGISTERs
  (`internal/b2bua/register.go`); FreeSWITCH must honor it to route the call back.
  In `fs_cli` run `sofia status profile internal reg` — the contact should show the
  sequencer as the path. If not, the `internal` sofia profile needs Path enabled.
- **No / one-way audio on the web (jsSIP) path — codec.** The sequencer's WebRTC
  (secured) leg is **opus-only and does not transcode**, so FreeSWITCH must also
  negotiate **opus** end to end. If `mod_opus` isn't loaded, FS falls back to G722,
  the relayed payloads don't match, and the call is connected-but-silent (or
  `488 Not Acceptable Here` if the browser offer has no opus). Provision installs
  `freeswitch-mod-opus`, autoloads it, and sets `global_codec_prefs=OPUS,...`.
  Verify during a call: `fs_cli -x "uuid_dump <uuid>" | grep -i codec` shows `OPUS`,
  and the browser's `about:webrtc` → `inbound-rtp` is non-empty.
- **No / one-way audio — network.** The sequencer advertises its ICE-lite host
  candidate at `media.public_address` (= `192.168.56.10`). Confirm the SDP answer
  carries that IP, and the Vagrant host can reach the VM (`ping 192.168.56.10`, UDP
  `30000-30100` open). WebRTC media only works from a browser on the Vagrant host
  (or via the SSH tunnel) — see the web jsSIP section.
- **No audio on a direct-to-FreeSWITCH call (1001↔1002).** FreeSWITCH must
  advertise `192.168.56.10` as its RTP IP — provisioning pins `local_ip_v4` in
  `/etc/freeswitch/vars.xml`. Check `fs_cli -x 'eval $${local_ip_v4}'`.
- **Sequencer fails to start.** `journalctl -u sip-sequencer` — usually a config
  error. `sip.listen` must be the concrete `192.168.56.10` (not `0.0.0.0`); the
  engine derives the SDP/Path/ICE host from it.

## Files

| File | Purpose |
|------|---------|
| `Vagrantfile` | VM, networks, port forwards, SIGNALWIRE_TOKEN passthrough |
| `provision.sh` | installs FreeSWITCH (native) + Go, builds binaries, installs units |
| `sequencer-config.yaml` | sequencer config (→ `/etc/voipstack-sip-sequencer/config.yaml`) |
| `systemd/*.service` | sequencer, recording app, webphone units |
| `web/index.html`, `web/phone.js` | jsSIP webphone |
