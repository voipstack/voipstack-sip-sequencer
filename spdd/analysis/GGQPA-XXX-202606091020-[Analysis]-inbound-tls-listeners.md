# SPDD Analysis: Inbound TLS listener

> Source: `requirements/[User-story-15]inbound-tls-listeners.md` (STORY-001-015), derived
> from `requirements/support-sip-tls.md`. Synced model: a single `tls.listen` listener
> running in parallel with the existing plain `sip.listen`; no idle-timeout (removed).

## Original Business Requirement

> Reproduced from `requirements/[User-story-15]inbound-tls-listeners.md`.

The `tls.listen` listener binds its port, accepts connections, wraps each in TLS using the
server context from `[STORY-001-014]`, performs the handshake (including mTLS enforcement
when configured), and audit-logs handshake/verification failures without exposing certificate
data. The plain listener is untouched; a config with no `tls.listen` opens no TLS listener.

(Full Business Value / Dependencies / Scope In / Scope Out / Acceptance Criteria AC1–AC6 /
Non-Functional Expectations as in the source story file.)

## Domain Concept Identification

### Existing Concepts (from codebase)
- **Engine.Run** (`engine.go:104`): registers SIP handlers then calls
  `e.srv.ListenAndServe(ctx, "udp", e.cfg.SIP.Listen)` — **one** listener, and it **blocks**.
  Adding a TLS listener means running a second listen call concurrently and blocking on both.
- **`startObservability`** (`engine.go:145`): the precedent for a second listener — a separate
  HTTP server started on its own goroutine off the SIP path, with bind errors logged. The TLS
  listener follows the same "second socket, own goroutine, log bind failure" shape, but unlike
  obs it is on the SIP path (shares `e.srv`).
- **`sipgo.Server`** (`e.srv`): the same server serves multiple transports — sipgo exposes a
  TLS listen entry point that takes a `*tls.Config`. One `Server`, two `ListenAndServe*`
  calls (udp + tls), shared handlers (`OnInvite`, etc.).
- **Config.TLS / resolved listener profile** (from 012/014): supplies the server context.
- **`Shutdown`** (`engine.go:164`): tears down obs + SIP server; must also stop the TLS
  listener cleanly.

### New Concepts Required
- **TLS listener** — a second `e.srv.ListenAndServe*(ctx, "tls", cfg.TLS.Listen, serverCtx)`
  bound in parallel with the UDP listener, sharing the existing SIP handlers.
- **Concurrent listen orchestration** — run UDP and TLS listeners together (e.g.
  `errgroup`/goroutines) and return when either fails or `ctx` is cancelled; today `Run`
  blocks on a single call.
- **Handshake-failure audit log** — peer address + reason, no certificate bytes (the mTLS
  rejection path surfaces from the server context's verification callback, `[STORY-001-014]`).

### Key Business Rules
- **Parallel by construction** (AC1, AC6): plain on `sip.listen`, TLS on `tls.listen`, both
  active; neither replaces the other.
- **Encrypted signaling processed identically** (AC2): once handshake completes, SIP handling
  is unchanged (handlers are shared) — only the transport differs.
- **mTLS rejects untrusted clients; plain unaffected** (AC3): rejection is enforced by the
  server context; failure isolated to the TLS listener.
- **No secret material in failure logs** (AC4).
- **Enabling TLS does not change plain behavior** (AC5); **omitting `tls.listen` ⇒ no TLS
  listener** (AC6).

## Strategic Approach

### Solution Direction
- **Make `Engine.Run` start both listeners.** Refactor the single blocking
  `srv.ListenAndServe(udp)` into a small concurrent runner: always start the UDP listener;
  when `cfg.TLS.Listen != ""`, obtain the server context from the provider and start the TLS
  listener too. Block until any listener returns an error or `ctx` is cancelled; propagate the
  first error. The shared `e.srv` means SIP handlers are registered once and serve both.
- **Server context sourced at startup** from the provider (`[STORY-001-014]`), built from the
  resolved `tls.listen` profile whose certificate was loaded eagerly (`[STORY-001-013]`).
- **Failure isolation**: a TLS handshake failure is a per-connection event inside sipgo's
  accept loop; it must not affect the UDP listener (separate listener) — satisfied by running
  them independently.

### Key Design Decisions
- **Concurrency primitive for two listeners**: raw goroutines + error channel vs.
  `golang.org/x/sync/errgroup` (already an indirect dep). → **Recommend `errgroup` with the
  run context** — cancels the sibling listener when one fails, clean shutdown, minimal code.
  Keep goroutine/channel ownership explicit (AGENTS.md, `go test -race`).
- **Where the server context is obtained**: in `Run` (lazy, just-in-time) vs. in `New`
  (validated at construction). → **Obtain in `New` / startup wiring** so a context-build
  failure aborts startup before binding (consistent with eager cert load); `Run` only binds.
- **sipgo TLS entry point (confirmed)**: `Server.ListenAndServeTLS(ctx, "tls", cfg.TLS.Listen,
  serverCtx)` (`server.go:170`; network `"tls"` supported). The server passes the `*tls.Config`
  directly — independent of the UA's client `tlsConfig` — so the inbound server context and the
  outbound client contexts do not collide. mTLS flows purely through the config's `ClientAuth`.
- **No idle timeout**: the synced requirement has no inactivity timeout — connection lifetime
  follows the dialog/SIP layer as today. (Removed the prior idle-timeout behavior.)

### Alternatives Considered
- **A second `sipgo.Server`/UA for TLS**: rejected — duplicates handler wiring and dialog
  caches; the same server serves multiple transports.
- **TLS termination in a reverse proxy in front**: rejected — the requirement terminates TLS
  in the sequencer (mTLS subjects, audit), and PRD keeps it self-contained.

## Risk & Gap Analysis

### Requirement Ambiguities
- **R1 — partial-startup policy.** If the TLS listener fails to bind (e.g. port in use) but
  UDP binds, does the process abort or run degraded (plain only)? Obs uses degraded+log;
  TLS is a primary security feature. Recommend **fail startup** on TLS bind error (an operator
  asked for TLS); confirm.
- **R2 — sipgo TLS API specifics (RESOLVED).** `Server.ListenAndServeTLS(ctx, "tls", addr,
  *tls.Config)` (`server.go:170`); mTLS flows through the config's `ClientAuth`. Confirmed
  against sipgo v1.4.0 source.

### Edge Cases
- `tls.listen` == `sip.listen` (same host:port): must be rejected (one is UDP, one is
  TCP/TLS, but a config error nonetheless) — validate at load (story 012) or at bind.
- TLS bind succeeds, certificate later unreadable: precluded by eager load (013) — cert is
  validated before bind.
- `ctx` cancellation mid-handshake: both listeners must stop and `Shutdown` must close the TLS
  listener (extend `engine.go:164`).
- Slow/stalled handshake from one client: must not block accepts on the other listener (NFR) —
  sipgo accept loop handles per-connection; verify no shared lock.

### Technical Risks
- **`Run` refactor from single-blocking to concurrent**: touches the engine's central run
  loop and `Shutdown`; must stay race-clean and preserve existing UDP behavior exactly (AC5).
  Medium risk — covered by existing engine tests + a new parallel-listener behavior test.
- **Testing TLS termination**: needs a TLS-capable fake client (real cert in testdata) hitting
  the TLS port, mirroring `transport_test.go`'s `newFakeUASTCP` pattern (a `newFakeUACTLS`).
  External peers are the only mock boundary; internal handlers tested for real (AGENTS.md).
- **No new data-integrity concern** beyond goroutine lifecycle.

### Acceptance Criteria Coverage
| AC# | Description | Addressable? | Gaps/Notes |
|-----|-------------|--------------|------------|
| AC1 | TLS + plain active simultaneously | Yes | Concurrent runner; shared `e.srv`. |
| AC2 | Valid TLS client signaling proceeds | Yes | Handlers shared; only transport differs. |
| AC3 | mTLS rejects untrusted; plain unaffected | Yes | Rejection from server ctx (014); listeners independent. |
| AC4 | Handshake failures logged, no cert data | Yes | Audit log peer+reason only. |
| AC5 | Enabling TLS doesn't change plain | Yes | UDP path untouched; behavior test. |
| AC6 | Omit `tls.listen` ⇒ no TLS listener | Yes | Conditional start on `cfg.TLS.Listen != ""`. |

**Summary:** All 6 ACs addressable. **R2 resolved** (`ListenAndServeTLS(ctx,"tls",addr,conf)`).
The real work is the **`Engine.Run` concurrent-listener refactor** (errgroup, shutdown,
race-clean) and a TLS-capable test client. Only open decision: R1 (partial-startup policy —
recommend fail-startup on TLS bind error).
