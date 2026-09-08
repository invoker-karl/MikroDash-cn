package session

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// readSource reads a file of this package, for the two source-reading checks
// below. They read the CURRENT source, which is the only way to tell a gate that
// still exists from one a refactor quietly removed.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return strings.Join(strings.Fields(string(b)), " ")
}

func contains(src, want string) bool {
	return strings.Contains(src, strings.Join(strings.Fields(want), " "))
}

// The debounce that replaced the fifteen-second ticker.
//
// ── WHY THIS IS WORTH A TEST OF ITS OWN ─────────────────────────────────────
//
// Phase 3.3 moved the dormancy supervisor off a clock and onto the scheduler's
// deliveries. Deliveries arrive several times a second on a busy router, so the
// whole correctness of that move is in the debounce: too eager and the
// supervisor walks nineteen payloads by reflection at ten hertz; too slow, or
// stuck, and it never judges again and no collector ever sleeps.
//
// Neither failure shows up anywhere. A supervisor that stops judging looks
// exactly like a router with nothing to report.
//
// `dormancyAt` is driven directly rather than through a Session with a router,
// because the property under test is the gate, not what a judgement decides —
// `internal/dormancy` owns that and has its own corpus.
func TestTheDeliveryDebounceIsTheOldInterval(t *testing.T) {
	var at atomic.Int64
	// The gate, lifted out of noteDelivery so it can be driven with a clock. It
	// must stay the same expression; TestTheDebounceGateMatchesTheCode below is
	// what says so.
	due := func(now int64) bool {
		last := at.Load()
		if last != 0 && now-last < dormancyTick.Milliseconds() {
			return false
		}
		return at.CompareAndSwap(last, now)
	}

	start := int64(1_000_000)
	if !due(start) {
		t.Fatal("the first delivery must judge: nothing has been judged yet")
	}
	for _, d := range []int64{1, 100, 5000, 14999} {
		if due(start + d) {
			t.Errorf("a delivery %dms after a judgement judged again; the floor is %v",
				d, dormancyTick)
		}
	}
	if !due(start + dormancyTick.Milliseconds()) {
		t.Errorf("a delivery exactly %v later did not judge — the supervisor would stall "+
			"for as long as deliveries kept arriving on that boundary", dormancyTick)
	}
}

// TestTheDebounceGateMatchesTheCode. The test above reimplements the gate, which
// is the one thing a test must not silently do: `noteDelivery` could be changed
// to judge on every delivery and the copy above would still pass.
//
// So the copy is checked against the source. Not clever, and it is the only
// thing standing between a reimplemented gate and a test that proves nothing.
func TestTheDebounceGateMatchesTheCode(t *testing.T) {
	src := readSource(t, "dormancy_run.go")
	for _, want := range []string{
		"last := s.dormancyAt.Load()",
		"now-last < dormancyTick.Milliseconds()",
		"s.dormancyAt.CompareAndSwap(last, now)",
	} {
		if !contains(src, want) {
			t.Errorf("noteDelivery no longer contains %q. The debounce test above is a COPY "+
				"of that gate; update both or it proves nothing.", want)
		}
	}
}

// TestTheSupervisorIsNoLongerAGoroutine. The point of 3.3 was that the ticker
// goes away, and a ticker quietly reinstated beside the delivery hook would be
// two clocks judging the same thing — the shape this rewrite has hit twice.
func TestTheSupervisorIsNoLongerAGoroutine(t *testing.T) {
	src := readSource(t, "dormancy_run.go")
	for _, gone := range []string{"time.NewTicker(dormancyTick)", "func (s *Session) runDormancy"} {
		if contains(src, gone) {
			t.Errorf("%q is back in dormancy_run.go. The supervisor rides deliveries now; a "+
				"clock beside them is a second one.", gone)
		}
	}
	if !contains(readSource(t, "session.go"), "s.judgeOnDelivery()") {
		t.Error("session.go no longer wires judgeOnDelivery, so nothing judges dormancy at all " +
			"— which looks exactly like a fleet with nothing to report")
	}
	_ = time.Second
}
