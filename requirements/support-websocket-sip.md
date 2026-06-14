# Support for SIP over WebSocket

## Overview
Add **WebSocket** as another inbound SIP transport, alongside the existing
**UDP / TCP / TLS** endpoints, so web-based SIP clients (webphones) built with
**jssip** or **sip.js** can connect without changing their application.

WebSocket is treated as a transport, not a special gateway: SIP signaling is
parsed and routed exactly as on the other transports, and the sequencer keeps
behaving as a **proxy / B2BUA**. **No transcoding** — codecs are negotiated end
to end between legs; the sequencer does not convert them.

## Requirements

### Signaling transport (the core of this work)
- **SIP over WebSocket** — RFC 7118. New inbound endpoint, peer to UDP/TCP/TLS:
  - **WS** (unencrypted) — development/testing only.
  - **WSS** (over TLS) — production.
- Negotiate the `sip` WebSocket subprotocol (`Sec-WebSocket-Protocol: sip`).
- **SIP Outbound** — RFC 5626. jssip/sip.js register with `;ob`, a reg-id and
  an instance-id over a single long-lived WebSocket flow; support outbound flow
  handling and connection reuse.
- **Path** — RFC 3327. Required so registrations through the WebSocket edge
  route back correctly.
- Keep-alive: honor WebSocket ping/pong and RFC 5626 CRLF keep-alives to hold
  the flow open through NAT/idle timeouts.

### Media (SRTP↔RTP bridge, no transcoding)
- **No transcoding.** Codecs pass through unchanged end to end — Opus stays
  Opus, PCMU stays PCMU. The sequencer never converts codecs.
- **But the webphone leg is not plain-RTP pass-through.** Browser media is
  always WebRTC: **DTLS-SRTP + ICE + rtcp-mux**, with no plain-RTP option in
  jssip/sip.js. The existing anchor is plain RTP today.
- Therefore the webphone-facing media leg **must terminate DTLS-SRTP and answer
  ICE-lite**, then **bridge to plain RTP** on the opposite leg. This is
  encrypt/decrypt + DTLS handshake + ICE — new media-plane work, not codec
  conversion.
- Candidate library for this bridge: **pion/webrtc** (`SettingEngine.SetLite(true)`
  + DTLS-SRTP), pure Go, no cgo.
- **Future-proof the bridge.** Only DTLS-SRTP↔RTP is required now, but design
  the media leg so an **SRTP↔SRTP proxy** (encrypted on both legs) can be added
  later without reworking the bridge — keep per-leg media security a
  configurable property of each leg, not hardcoded to "webphone = SRTP, other =
  RTP".

### NAT traversal
- **ICE (ICE-lite)** — RFC 8445. The media anchor is publicly reachable and
  runs **ICE-lite** (answers/validates candidates; gathers host candidates
  only), matching Asterisk, FreeSWITCH, and Janus.
- **Trickle ICE** — RFC 8838. Accept trickled candidates and end-of-candidates.
- STUN connectivity checks are inherent to ICE and handled by the ICE-lite
  implementation — no separate STUN deployment required.
- Server is the public RTP anchor, so **client-side TURN is unnecessary**;
  TURN relaying is out of scope.

## Compatibility
- Seamless client migration — existing jssip and sip.js integrations work with
  no code changes.
- Standard SIP semantics preserved; WebSocket is just the transport.

## Out of Scope (for now)
- **SRTP↔SRTP proxy** — not required yet, but the media bridge must be designed
  so it can be added later without rework (see Media section).
- **Transcoding** — codecs are negotiated end to end, never converted.
- **TURN relay** — the server is a public media anchor.
- Full (non-lite) ICE on the server side.
- SIP extensions beyond the RFCs listed above.
