// Minimal jsSIP UA for the sequencer test box.
// Signaling goes over WebSocket to the sequencer (ws://localhost:8080); the
// sequencer forwards REGISTER/INVITE to FreeSWITCH and anchors WebRTC<->RTP media.
'use strict';

const $ = (id) => document.getElementById(id);
const statusEl = $('status');
const remote = $('remote');

let ua = null;
let session = null;

function setStatus(msg) {
  statusEl.textContent = msg;
  console.log('[phone]', msg);
}

function attachRemoteAudio(s) {
  s.connection.addEventListener('track', (e) => {
    remote.srcObject = e.streams[0];
  });
}

// NOTE: the sequencer's WebRTC (secured) leg is OPUS-only — offering it without
// opus is rejected with 488. Since the sequencer anchors media WITHOUT
// transcoding, FreeSWITCH must also negotiate opus (load mod_opus + put OPUS
// first in global_codec_prefs). With both legs on opus the relay works. So the
// browser offer is left untouched (opus included).

function wireSession(s) {
  session = s;
  attachRemoteAudio(s);
  $('hangup').disabled = false;
  s.on('ended',     () => { setStatus('call ended'); resetCallButtons(); });
  s.on('failed',   (e) => { setStatus('call failed: ' + (e.cause || '')); resetCallButtons(); });
  s.on('confirmed', () => setStatus('in call'));
}

function resetCallButtons() {
  session = null;
  $('hangup').disabled = true;
  $('answer').disabled = true;
  $('call').disabled = !(ua && ua.isRegistered());
}

$('register').onclick = () => {
  const socket = new JsSIP.WebSocketInterface($('ws').value);
  const domain = $('domain').value;
  const ext = $('ext').value;
  ua = new JsSIP.UA({
    sockets: [socket],
    uri: `sip:${ext}@${domain}`,
    password: $('pw').value,
    register: true,
    session_timers: false,
  });

  ua.on('connected',    () => setStatus('ws connected'));
  ua.on('disconnected', () => setStatus('ws disconnected'));
  ua.on('registered',   () => { setStatus('registered as ' + ext); $('call').disabled = false; $('unregister').disabled = false; $('register').disabled = true; });
  ua.on('unregistered', () => { setStatus('unregistered'); $('call').disabled = true; });
  ua.on('registrationFailed', (e) => setStatus('register failed: ' + (e.cause || '')));

  // Incoming call.
  ua.on('newRTCSession', (e) => {
    if (e.originator === 'remote') {
      setStatus('incoming from ' + e.request.from.uri.user);
      wireSession(e.session);
      $('answer').disabled = false;
    }
  });

  ua.start();
};

$('unregister').onclick = () => {
  if (ua) ua.stop();
  $('register').disabled = false;
  $('unregister').disabled = true;
  $('call').disabled = true;
  setStatus('stopped');
};

$('call').onclick = () => {
  const target = `sip:${$('target').value}@${$('domain').value}`;
  setStatus('calling ' + target);
  const s = ua.call(target, {
    mediaConstraints: { audio: true, video: false },
    pcConfig: { iceServers: [] }, // ICE-lite peer on the sequencer; no STUN needed on LAN
  });
  wireSession(s);
};

$('answer').onclick = () => {
  if (session) {
    session.answer({ mediaConstraints: { audio: true, video: false }, pcConfig: { iceServers: [] } });
    $('answer').disabled = true;
  }
};

$('hangup').onclick = () => {
  if (session) session.terminate();
};
