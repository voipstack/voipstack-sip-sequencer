package b2bua

import "testing"

// registerMedia must refuse on a tearing-down call (so the caller releases the media it
// acquired — teardown's release hook never saw it) and must not run fn.
func TestRegisterMediaRefusedWhenTearingDown(t *testing.T) {
	c := &Call{state: stateTearingDown}
	if c.registerMedia(func() { t.Fatal("fn ran on a tearing-down call") }) {
		t.Fatal("registerMedia returned true for a tearing-down call")
	}
}

// registerMedia runs fn and reports success for a live (setup or established) call.
func TestRegisterMediaRunsWhenLive(t *testing.T) {
	for _, st := range []CallState{stateSetup, stateEstablished} {
		c := &Call{state: st}
		ran := false
		if !c.registerMedia(func() { ran = true }) || !ran {
			t.Fatalf("state %q: registerMedia did not run fn", st)
		}
	}
}

// When a call tears down during setup before its media is tracked, the caller's bail path
// returns the acquired RTP port pairs to the allocator instead of leaking them.
func TestSetupTeardownReleasesAcquiredPorts(t *testing.T) {
	ports := newPortAllocator(20000, 20020)
	p1, _ := ports.Acquire()
	p2, _ := ports.Acquire()
	release := mediaReleaser(&MediaSession{}, ports, p1, p2)

	c := &Call{state: stateTearingDown}
	if c.registerMedia(func() { t.Fatal("must not register on a torn-down call") }) {
		t.Fatal("expected registerMedia to refuse")
	}
	release() // caller's bail cleanup

	freed := map[int]bool{}
	for i := 0; i < 2; i++ {
		p, err := ports.Acquire()
		if err != nil {
			t.Fatalf("port not reusable after bail release: %v", err)
		}
		freed[p.RTP] = true
	}
	if !freed[p1.RTP] || !freed[p2.RTP] {
		t.Fatalf("acquired pairs leaked: reusable=%v want %d and %d", freed, p1.RTP, p2.RTP)
	}
}
