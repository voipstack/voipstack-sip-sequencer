# Story Decomposition: SIP TLS transport support (story 4/5)

> Part of the TLS decomposition. Module overview and INVEST analysis in
> `[User-story-12]tls-configuration.md`. Derived from `requirements/support-sip-tls.md`.

---

## [STORY-001-015] Inbound TLS listener

### Background
Upstream SIP clients (phones, edge proxies) must be able to reach the sequencer over TLS
while existing plain clients keep working unchanged. The plain `sip.listen` listener and
the new `tls.listen` listener bind separate ports and run in parallel by construction. This
story makes the `tls.listen` listener real: it binds the configured port, accepts
connections, wraps each in TLS using the server context from `[STORY-001-014]`, performs
the handshake (including mTLS enforcement when the profile requires it), and audit-logs
handshake/verification failures without exposing certificate data. The plain listener is
untouched, and a config with no `tls.listen` opens no TLS listener at all.

Key points:
- Business value: encrypted inbound signaling for clients that require it, with no
  disruption to clients still on plain transport.
- Consumes the server context boundary; first place TLS actually terminates on the wire.
- Needed now because inbound TLS is a primary requirement (listen on a TLS port in parallel
  with the plain port).

### Business Value
- Provide upstream SIP clients an encrypted, optionally mutually-authenticated way to reach
  the sequencer.
- Support a migration path — the TLS and plain listeners run simultaneously, so clients move
  to TLS at their own pace.
- Enable an operator to enforce that only trusted clients connect (mTLS), with an audit
  trail of rejected handshakes.

### Dependencies and Assumptions
- **Prerequisites:** `[STORY-001-012]` (resolved listener profile), `[STORY-001-014]`
  (server-side TLS context). Existing plain SIP listening from earlier module-001 stories.
- **Data assumptions:** The `tls.listen` block names a resolved profile; its certificate
  loads successfully via the provider at startup.
- **Integration points:** Upstream SIP clients over the network (the external peers).
- **Business constraints:** TLS and plain listeners must run in parallel; enabling TLS must
  not change plain-listener behavior. Omitting `tls.listen` opens no TLS listener.

### Scope In
- Open a TLS listener on the `tls.listen` port, alongside the existing plain `sip.listen`
  listener (parallel, separate ports).
- On accept, wrap the connection in TLS using the listener's server context and complete the
  handshake before treating it as a SIP connection.
- Enforce mTLS when the profile requires it — reject and close connections whose client
  certificate is absent, invalid, of a disallowed subject, or fails chain/date checks.
- Audit-log handshake/verification failures (peer address, reason) with no certificate data.

### Scope Out
- Outbound TLS to applications / next hop — `[STORY-001-016]`.
- Building the server context / verification policy — `[STORY-001-014]`.
- SIP dialog/B2BUA behavior once the connection is up — already owned by earlier module-001
  stories; this story only delivers the TLS-terminated transport beneath them.
- Certificate reload without restart (future consideration).

### Acceptance Criteria

#### AC1: TLS and plain listeners are active simultaneously
**Given** `sip.listen: 0.0.0.0:5060` and a `tls` block `listen: 0.0.0.0:5061`
`tls_profile: inbound`
**When** the process is running
**Then** a plain client reaching 5060 is served as before **and** a TLS client reaching 5061
completes a TLS handshake — both listeners are active at once.

#### AC2: A valid TLS client's signaling proceeds over the encrypted connection
**Given** the TLS listener on 5061 and a client presenting a valid TLS session
**When** the client sends SIP signaling over the established TLS connection
**Then** the signaling is processed exactly as it would be over plain transport, but carried
encrypted.

#### AC3: mTLS listener rejects an untrusted client without disturbing plain calls
**Given** a TLS listener using an mTLS profile (`verify_peer: true`,
`verify_subjects: [CN=phone.internal]`) and, separately, the plain listener
**When** a client connects to the TLS port presenting no certificate (or a disallowed
subject)
**Then** the handshake is rejected and the connection closed, the failure is audit-logged,
and calls arriving on the plain listener continue to be served normally.

#### AC4: Handshake failures are logged without certificate data
**Given** TLS logging at debug level and a client whose handshake fails verification
**When** the operator inspects the logs
**Then** the failure is recorded with the peer address and a reason, and no certificate body
or key material appears in any log line.

#### AC5: Enabling TLS does not change plain-listener behavior
**Given** a configuration that previously had only the plain listener, with a `tls.listen`
listener now added
**When** plain clients connect as before
**Then** their behavior is unchanged and unaffected by the presence of the TLS listener.

#### AC6: Omitting `tls.listen` opens no TLS listener
**Given** a config with no `tls` block
**When** the process starts
**Then** only the plain `sip.listen` listener is opened; no TLS port is bound.

#### Non-Functional Expectations
- A failed or slow TLS handshake from one client must not block acceptance of connections on
  the other listener.
- Plain (non-TLS) connections must carry no TLS-related overhead.
