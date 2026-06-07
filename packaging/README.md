# Deploying voipstack-sip-sequencer

A single static binary run under systemd on one host. No build toolchain, container,
or migration step is needed on the target machine.

## Build the artifact (on a build host)

```sh
make build                       # dist/sip-sequencer (VERSION from git describe, or "dev")
make release VERSION=v1.2.3      # dist/sip-sequencer-v1.2.3-linux-amd64 + .sha256
make deb     VERSION=v1.2.3      # dist/voipstack-sip-sequencer_1.2.3_amd64.deb (needs nfpm)
```

The build is static (`CGO_ENABLED=0`) and path-stripped (`-trimpath`). The same tag built
with the same pinned Go toolchain (`go` directive in `go.mod`, currently **1.23.6**)
produces the same contents. Verify the binary is static:

```sh
file dist/sip-sequencer    # ... statically linked
ldd  dist/sip-sequencer    # not a dynamic executable
```

## Install (Debian package — preferred)

The `.deb` installs the binary (`/usr/bin`), the systemd unit (`/lib/systemd/system`),
and an initial config (`/etc/voipstack-sip-sequencer/config.yaml`), and enables the
service on boot. The config is a dpkg **conffile** — your edits survive upgrades.

```sh
sudo dpkg -i dist/voipstack-sip-sequencer_1.2.3_amd64.deb
# or: sudo apt install ./dist/voipstack-sip-sequencer_1.2.3_amd64.deb

# edit the seeded config (set next_hop etc.), then start
sudoedit /etc/voipstack-sip-sequencer/config.yaml
sudo systemctl start voipstack-sip-sequencer
```

The package enables but does not auto-start the service, because the seeded config has a
placeholder `next_hop`. Inspect the payload before installing with `dpkg -c <file>.deb`.

## Install (manual — no package)

```sh
# 1. binary
sudo cp dist/sip-sequencer /usr/bin/

# 2. config
sudo mkdir -p /etc/voipstack-sip-sequencer
sudo cp packaging/config.example.yaml /etc/voipstack-sip-sequencer/config.yaml
# edit /etc/voipstack-sip-sequencer/config.yaml for your host

# 3. systemd unit
sudo cp packaging/systemd/voipstack-sip-sequencer.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now voipstack-sip-sequencer
```

## Verify

```sh
systemctl status voipstack-sip-sequencer
journalctl -u voipstack-sip-sequencer -f
```

If `observability.listen` is set, health and metrics are exposed there:

```sh
curl http://<observability.listen>/health
curl http://<observability.listen>/metrics
```

## Upgrade

No migration — the process is stateless on disk.

```sh
# Debian package: conffile preserves your edited config
sudo dpkg -i dist/voipstack-sip-sequencer_1.3.0_amd64.deb
sudo systemctl restart voipstack-sip-sequencer

# Manual: replace the binary and restart
sudo cp dist/sip-sequencer /usr/bin/
sudo systemctl restart voipstack-sip-sequencer
```

## Remove / purge (Debian package)

```sh
sudo apt remove voipstack-sip-sequencer    # stops + disables; keeps /etc config
sudo apt purge  voipstack-sip-sequencer    # also removes /etc/voipstack-sip-sequencer
```

## Capabilities note

SIP on the privileged port `:5060` requires `CAP_NET_BIND_SERVICE`, which the unit grants
to the non-root service user. If your `sip.listen` uses a high (>1024) port, you can remove
`AmbientCapabilities=CAP_NET_BIND_SERVICE` from the unit.

## Bad-config behaviour

A config that fails validation makes the service exit non-zero with the error on stderr.
After a few restart attempts (`StartLimitBurst`) systemd leaves the unit in `failed`
state — it does **not** loop forever. The error is visible in:

```sh
systemctl status voipstack-sip-sequencer    # Active: failed
journalctl -u voipstack-sip-sequencer       # error: missing required key "..."
```
