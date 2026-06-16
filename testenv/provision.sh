#!/usr/bin/env bash
# Provision the sequencer test box (everything native in the VM, no containers).
# Idempotent: safe to re-run with `vagrant provision`.
set -euo pipefail

LAB_IP=192.168.56.10
GO_VER=1.24.0
# Must not be "1234": vanilla FreeSWITCH intercepts all calls with a "change the
# default_password" nag + 10s sleep while default_password is still 1234.
LAB_PASSWORD=voipstacklab
export DEBIAN_FRONTEND=noninteractive
export PATH=$PATH:/usr/local/go/bin

echo "== packages =="
apt-get update -y
apt-get install -y --no-install-recommends \
  ca-certificates curl git make gnupg2 wget python3 tar

echo "== FreeSWITCH (native, SignalWire repo) =="
# FreeSWITCH is no longer in Debian; SignalWire's repo is the supported source and
# requires a free Personal Access Token (https://signalwire.com -> Personal Access
# Token). Pass it in via the SIGNALWIRE_TOKEN env (the Vagrantfile forwards it from
# your host environment).
: "${SIGNALWIRE_TOKEN:?set SIGNALWIRE_TOKEN to a free SignalWire Personal Access Token and re-run}"

# If a previous (containerized) run left a FreeSWITCH container holding :5060, drop it.
if command -v docker >/dev/null 2>&1; then docker rm -f freeswitch >/dev/null 2>&1 || true; fi

if ! command -v freeswitch >/dev/null 2>&1; then
  wget --http-user=signalwire --http-password="$SIGNALWIRE_TOKEN" \
    -qO /usr/share/keyrings/signalwire-freeswitch-repo.gpg \
    https://freeswitch.signalwire.com/repo/deb/debian-release/signalwire-freeswitch-repo.gpg
  echo "machine freeswitch.signalwire.com login signalwire password $SIGNALWIRE_TOKEN" \
    > /etc/apt/auth.conf.d/freeswitch.conf
  chmod 600 /etc/apt/auth.conf.d/freeswitch.conf
  echo "deb [signed-by=/usr/share/keyrings/signalwire-freeswitch-repo.gpg] https://freeswitch.signalwire.com/repo/deb/debian-release/ bookworm main" \
    > /etc/apt/sources.list.d/freeswitch.list
  apt-get update -y
  # meta-vanilla pulls mod_sofia, codecs, the dialplan mods, and the vanilla config
  # (users 1000-1019, password 1234, echo test on 9196) into /etc/freeswitch.
  apt-get install -y freeswitch-meta-vanilla
fi

# Vanilla auto-detects local_ip_v4 as the default-route (NAT) iface, which the host
# cannot reach -> no audio. Pin it to the host-only IP. Insert before the
# domain=$${local_ip_v4} line, i.e. right after the opening <include>.
if ! grep -q "local_ip_v4=${LAB_IP}" /etc/freeswitch/vars.xml; then
  sed -i "0,/<include>/s##<include>\n  <X-PRE-PROCESS cmd=\"set\" data=\"local_ip_v4=${LAB_IP}\"/>#" /etc/freeswitch/vars.xml
fi
# Vanilla sets external_*_ip to STUN, which NAT-detects the public IP and makes
# FreeSWITCH advertise an address the LAN can't use. Pin them to the host-only IP.
sed -i "s#external_rtp_ip=stun:stun.freeswitch.org#external_rtp_ip=${LAB_IP}#; s#external_sip_ip=stun:stun.freeswitch.org#external_sip_ip=${LAB_IP}#" /etc/freeswitch/vars.xml
# Change default_password off 1234 so the vanilla default-password nag dialplan
# (warning + 10s sleep, blocks call routing) does not fire. All 1000-1019 users
# inherit $${default_password}.
sed -i "s/default_password=1234/default_password=${LAB_PASSWORD}/" /etc/freeswitch/vars.xml
# The sequencer's WebRTC (secured) leg is OPUS-only and does NOT transcode, so
# FreeSWITCH must also negotiate opus or webphone calls get 488 / no audio.
# mod_opus is not loaded by the vanilla install — install, autoload, and prefer it.
apt-get install -y freeswitch-mod-opus || true
if ! grep -qE '^[[:space:]]*<load module="mod_opus"' /etc/freeswitch/autoload_configs/modules.conf.xml; then
  sed -i 's#</modules>#  <load module="mod_opus"/>\n</modules>#' /etc/freeswitch/autoload_configs/modules.conf.xml
fi
sed -i 's#global_codec_prefs=[^"]*#global_codec_prefs=OPUS,PCMU,PCMA,G722#' /etc/freeswitch/vars.xml
systemctl enable --now freeswitch
systemctl restart freeswitch

echo "== Go ${GO_VER} =="
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VER}"; then
  curl -fsSL "https://go.dev/dl/go${GO_VER}.linux-amd64.tar.gz" -o /tmp/go.tgz
  rm -rf /usr/local/go
  tar -C /usr/local -xzf /tmp/go.tgz
fi

echo "== build sip-sequencer + recording-app =="
cd /vagrant
export GOCACHE=/root/.cache/go-build GOPATH=/root/go
# Build straight into /usr/local/bin. The 9p synced folder is read-only to the
# guest, so we must NOT write dist/ inside it (which `make build` would do).
CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o /usr/local/bin/sip-sequencer ./cmd/sip-sequencer
( cd applications/recording && CGO_ENABLED=0 go build -trimpath -o /usr/local/bin/recording-app . )

echo "== config + systemd units =="
mkdir -p /etc/voipstack-sip-sequencer
install -m0644 /vagrant/testenv/sequencer-config.yaml /etc/voipstack-sip-sequencer/config.yaml
install -d -m0755 /var/lib/webphone
install -m0644 /vagrant/testenv/web/index.html /vagrant/testenv/web/phone.js /var/lib/webphone/
# Vendor JsSIP locally (the CDN path was unreliable / MIME-blocked).
if [ ! -s /var/lib/webphone/jssip.min.js ]; then
  curl -fsSL https://raw.githubusercontent.com/versatica/JsSIP/3.10.1/dist/jssip.min.js -o /var/lib/webphone/jssip.min.js
fi
chmod 0644 /var/lib/webphone/jssip.min.js
install -m0644 /vagrant/testenv/systemd/sip-sequencer.service /etc/systemd/system/
install -m0644 /vagrant/testenv/systemd/recording-app.service /etc/systemd/system/
install -m0644 /vagrant/testenv/systemd/webphone.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now recording-app sip-sequencer webphone
systemctl restart recording-app sip-sequencer webphone

echo
echo "== ready =="
echo "  FreeSWITCH  : ${LAB_IP}:5060/udp   (users 1000-1019, password ${LAB_PASSWORD}, echo test 9196)"
echo "  sequencer   : ${LAB_IP}:5080/udp   ws ${LAB_IP}:8080   metrics :9090"
echo "  webphone    : http://localhost:8090   (forwarded)"
