# AGENTS.md — voipstack-sip-sequencer
Coding guidelines for agents working in this repository. Read before writing code.
Project: a SIP **application sequencer** in **Go** — a B2BUA that chains each call
through an ordered sequence of independent external SIP application servers (static
YAML), anchoring RTP. Transcription is one such application, not the core. See
`PRD.md` for product scope.
## Paradigm: simple and functional
- Prefer a **functional style**: pure functions, explicit inputs and outputs, no
  hidden state. A function's result should depend only on its arguments.
- Push side effects (I/O, sockets, clock, randomness) to the **edges** of the system.
  Keep the core — SDP parsing, call/leg correlation, codec mapping, id minting — pure
  and easy to test without a network.
- Avoid shared mutable state. When state is required (RTP session tables), make it
  small, owned by one component, and accessed through a narrow interface.
- Pass dependencies in (functions, small interfaces). No global singletons, no
  package-level mutable vars for behavior.
- Favor immutable values. Return new values instead of mutating arguments.
- Composition over inheritance/embedding-for-reuse. Build behavior from small
  functions.
## Design: Kent Beck simple design
Code must pass these four rules, in priority order:
1. **Passes the tests** — it works, proven by tests.
2. **Reveals intent** — names and structure say what and why. No comment needed to
   explain what a well-named function already says.
3. **No duplication** — say each thing once (DRY). Extract the shared idea.
4. **Fewest elements** — no class, interface, layer, or abstraction that isn't paying
   for itself right now.
Corollaries:
- **YAGNI** — build for today's requirement, not an imagined future one. No
  speculative generality, no config flags for features that don't exist.
- Smallest thing that could work first; refactor when a second case actually arrives.
- Delete dead code. Don't keep "might need it" branches.
## BDD: behavior-driven
- Tests describe **behavior**, not implementation. Test what the component does, not
  how.
- Structure each test **Given / When / Then** (arrange / act / assert). Name tests by
  the behavior: `TestForkPausesOnHold`, `TestReINVITEUpdatesForkRemote`.
- Drive features from the outside in: start from the observable behavior in `PRD.md`
  (e.g. "both legs arrive as one stereo session"), write the behavior test, then make
  it pass.
- One behavior per test. A failing test name should tell you what broke.
## Testing: high coverage only
- **Only add a test if it raises real coverage of behavior.** No tests that restate
  the implementation, no trivial getter/setter tests, no tests written to pad a number.
- Aim for high coverage of the **core logic** (correlation, SDP/codec handling, id
  minting, lifecycle/event rules). These are pure — cover them thoroughly.
- A test must be able to fail for a real reason. If it can't, delete it.
- Cover edge cases that matter: re-INVITE, hold/resume, REFER re-point, fork failure
  (best-effort, must not affect the call).
## Mocking: mock only external services
- **Do NOT mock internal code.** Internal functions, types, and packages are tested
  through their real implementations. If internal code is hard to test without mocks,
  that's a design smell — make the core pure and pass dependencies in instead.
- **Mock external services only** — things the process talks to over the network and
  does not own: the **WebSocket transcription server**, the **webhook endpoint**, and
  remote SIP/RTP peers where needed.
- **Database, memcache, and similar backing stores are NOT mocked.** Test against a
  real instance (local/ephemeral/container). Treat them as part of the system under
  test, not as an external service to stub.
- Prefer real fakes over assertion-heavy mocks: a small in-memory WebSocket/webhook
  server you can inspect beats a mock framework recording calls.
## Go specifics
- `gofmt` / `go vet` clean. Idiomatic Go.
- Errors are values — return and handle them; wrap with context (`fmt.Errorf("...: %w")`).
- Small interfaces, defined at the **consumer**, not the producer.
- Use `context.Context` for cancellation/lifecycle on anything long-lived.
- Concurrency: clear ownership of each goroutine and channel; no data races
  (`go test -race`).
## Definition of done
- Behavior test exists and passes.
- `go build ./...`, `go vet ./...`, `gofmt`, and `go test -race ./...` all pass.
- Only external services mocked; internal code and DB/cache tested for real.
- Code passes the four simple-design rules above.