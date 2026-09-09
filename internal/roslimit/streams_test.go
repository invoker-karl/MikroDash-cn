package roslimit

import "testing"

// The stream counter. It exists because `Acquire` caps in-flight COMMANDS and a
// stream takes no slot, so until Track B this process could hold any number of
// channels on a router and report nothing about it — against the bottleneck the
// project documents as concurrent channels.
//
// AN INSTRUMENT, NOT A LIMIT. B.0b searched to 24 concurrent channels on live
// hardware and found no ceiling, no starvation and no CPU trend, so enforcing a
// bound would enforce one nobody has observed.

func TestAnOpenStreamIsCountedAndReleased(t *testing.T) {
	Reset()
	if n := OpenStreams("r1"); n != 0 {
		t.Fatalf("a fresh router reports %d open stream(s)", n)
	}

	a := StreamOpened("r1")
	b := StreamOpened("r1")
	defer StreamOpened("r2")()

	if n := OpenStreams("r1"); n != 2 {
		t.Errorf("r1 holds %d, want 2", n)
	}
	if n := OpenStreams("r2"); n != 1 {
		t.Errorf("r2 holds %d, want 1 — the count is not per router", n)
	}

	a()
	if n := OpenStreams("r1"); n != 1 {
		t.Errorf("after one release r1 holds %d, want 1", n)
	}
	b()
	if n := OpenStreams("r1"); n != 0 {
		t.Errorf("after both releases r1 holds %d, want 0", n)
	}
}

// TestADoubleReleaseDoesNotDiscardAnotherStream.
//
// A collector torn down on both a blur and a disconnect releases twice, and this
// app does that routinely.
//
// ── THE OBVIOUS TEST FOR THIS DOES NOT WORK, AND THAT IS WORTH RECORDING ────
//
// Releasing a LONE stream twice proves nothing: the second decrement takes a
// missing key to -1, the `<= 0` branch deletes it again, and `OpenStreams`
// returns 0 for a missing key either way. Written that way the test passed
// against a release with no `sync.Once` at all — it was measuring the delete,
// not the idempotency.
//
// The case that distinguishes them is a double release while ANOTHER stream is
// still open: without `once` the count walks 2 -> 1 -> 0, the entry is deleted,
// and the instrument reports zero channels on a router that is holding one. That
// is the failure that matters, because the whole point of this counter is that
// B.4 reads it to decide whether a collector's stream is costing anything.
func TestADoubleReleaseDoesNotDiscardAnotherStream(t *testing.T) {
	Reset()
	first := StreamOpened("r1")
	defer StreamOpened("r1")() // still held throughout

	if n := OpenStreams("r1"); n != 2 {
		t.Fatalf("r1 holds %d, want 2", n)
	}
	first()
	first()
	first()
	if n := OpenStreams("r1"); n != 1 {
		t.Errorf("after releasing ONE stream three times r1 reports %d, want 1 — the "+
			"extra releases discarded a channel that is still open, and the "+
			"instrument now under-reports what this process costs the router", n)
	}
}

// TestStreamsTakeNoCommandSlot is the property the instrument exists because of:
// `Do` is gated and this is not. If a stream ever started taking a slot, a chart
// on screen could block a collector's read and the eight-slot budget would be
// spent on channels that are not commands.
func TestStreamsTakeNoCommandSlot(t *testing.T) {
	Reset()
	defer StreamOpened("r1")()
	if n := InFlight("r1"); n != 0 {
		t.Errorf("an open stream holds %d command slot(s); it must hold none", n)
	}
}

// TestAnEmptyRouterIdIsNotCounted, matching Acquire. A router with no id is a
// test fixture or a session being torn down, and counting those would report
// channels against a router that does not exist.
func TestAnEmptyRouterIdIsNotCounted(t *testing.T) {
	Reset()
	defer StreamOpened("")()
	if n := OpenStreams(""); n != 0 {
		t.Errorf("an empty id was counted (%d)", n)
	}
}

// TestResetClearsTheLevel. `counts` and `menus` are counters and decay to zero
// on their own; this is a LEVEL and does not, so a Reset that missed it would
// leave one test's channels counted against every test after it.
func TestResetClearsTheLevel(t *testing.T) {
	Reset()
	StreamOpened("r1") // deliberately never released
	Reset()
	if n := OpenStreams("r1"); n != 0 {
		t.Errorf("Reset left %d stream(s) on r1; a level that survives Reset is "+
			"counted against every later test in the package", n)
	}
}
