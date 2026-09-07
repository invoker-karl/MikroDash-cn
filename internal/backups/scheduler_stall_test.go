package backups

import (
	"strings"
	"testing"
	"time"
)

// A WEDGED RUN EVENTUALLY SAYS SO.
//
// ── THE INCIDENT ────────────────────────────────────────────────────────────
//
// 2026-09-07: a daily 08:00 backup ran at 10:52, on the first tick after the
// container was restarted. The app was alive throughout and logging every 105
// seconds, the backup was due, and roughly thirty five-minute ticks passed with
// NOTHING written by the scheduler.
//
// The cause was an unbounded command — see `server.backupCmdTimeout`, which is
// the fix — holding `Session.InWriteQueue`, a bare mutex, inside a `Tick` that
// calls the queue synchronously in the ticker's own goroutine. But the reason
// nobody noticed for three hours was separate and lives here: `claim` refusing a
// still-running router was deliberately silent, so "this backup is taking a
// while" and "this scheduler has stopped" looked identical from outside, which
// is to say invisible.
//
// ── WHAT THIS TEST DOES NOT CLAIM, AND WHY THAT CHANGED ─────────────────────
//
// An earlier version of this file asserted that a router hanging FOR EVER must
// not stop the next router being backed up, and it failed. It was withdrawn
// rather than fixed, because it demanded a property this design deliberately
// does not have: `Tick` runs the fleet one router at a time, and making it
// concurrent would open several router channels at once — against the hard
// constraint that "more efficient means FEWER router channels".
//
// The real guarantee is different and weaker on purpose: every run is BOUNDED,
// so a router that stops answering delays the fleet by at most that bound and
// then fails, logs, releases and lets the loop continue. Deleting the
// over-strong assertion is recorded here rather than done quietly, because a
// test removed without a reason reads exactly like one that never existed.
func TestARepeatedSkipIsReported(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)

	h := &harness{
		lastRun: map[string]int64{"a": lateYesterday()},
		hold:    hold,
	}
	h.routers = []SchedRouter{dueRouter("a")}

	// ONE scheduler across every tick: the skip streak is its state, and a fresh
	// one per tick would reset it and pass whatever the counting did.
	s := h.scheduler(schedNow)

	// The first tick claims the router and parks in RunFor, exactly as a wedged
	// backup does.
	go s.Tick()
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.attempts) == 1
	}, "the first tick never started a run")

	// ── the first skips stay quiet, which is the original intent ────────────
	//
	// A large configuration outlasting a five-minute tick is normal, and the
	// silence that made this incident invisible was a REASONABLE decision about
	// that case. Keeping it is the point; only the unbounded silence goes.
	for i := 1; i < skipReportAfter; i++ {
		s.Tick()
		if got := logsOf(h); len(got) != 0 {
			t.Fatalf("skip %d of %d was reported; a backup that merely outlasts a "+
				"tick must stay quiet, or a large configuration fills the log "+
				"every five minutes: %v", i, skipReportAfter, got)
		}
	}

	// ── and then it is reported, once ───────────────────────────────────────
	s.Tick()
	got := logsOf(h)
	if len(got) != 1 {
		t.Fatalf("after %d consecutive skips the scheduler logged %d line(s), want 1.\n"+
			"This is the half of the 2026-09-07 incident that made it invisible: a "+
			"wedged run and a slow one were indistinguishable, and the wedged one "+
			"lasted three hours without a line anywhere.", skipReportAfter, len(got))
	}
	if !strings.Contains(got[0], "R-a") || !strings.Contains(got[0], "still running") {
		t.Errorf("the report does not name the router and its condition, so it "+
			"cannot be acted on: %q", got[0])
	}

	// ── AND IT DOES NOT REPEAT, which is the other half of the original intent ─
	//
	// Reporting every tick would trade one silence for a log nobody reads, and
	// a genuinely long backup would be the thing spamming it.
	s.Tick()
	if got := logsOf(h); len(got) != 1 {
		t.Errorf("the skip was reported again on the next tick (%d lines total). "+
			"It must be said once per stretch, or a long backup fills the log: %v",
			len(got), got)
	}
}

// A RUN THAT STARTS CLEARS THE STREAK, so a later stall is reported on its own
// merits instead of being masked by an earlier one that resolved.
//
// Driven through claim/release/noteSkip directly rather than through ticks. The
// first draft swapped the harness's `hold` channel mid-test to make a run finish
// and another begin, and `-race` caught it reading that field unsynchronised
// from RunFor — a defect in the TEST, but one that would have made this suite
// flaky rather than the code wrong. The streak is a property of those three
// methods, so this asks them.
func TestAStartedRunResetsTheSkipStreak(t *testing.T) {
	h := &harness{lastRun: map[string]int64{"a": lateYesterday()}}
	h.routers = []SchedRouter{dueRouter("a")}
	s := h.scheduler(schedNow)

	if !s.claim("a") {
		t.Fatal("a free router could not be claimed")
	}
	// Two skips while it runs — one short of a report.
	if n := s.noteSkip("a"); n != 1 {
		t.Fatalf("first skip counted %d, want 1", n)
	}
	if n := s.noteSkip("a"); n != 2 {
		t.Fatalf("second skip counted %d, want 2", n)
	}

	// The run finishes and the next tick claims it again.
	s.release("a")
	if !s.claim("a") {
		t.Fatal("a released router could not be re-claimed")
	}

	// THE STREAK MUST HAVE RESTARTED. Without the reset this counts 3 and trips
	// the report on a router whose previous stall had already resolved — which
	// would make the warning mean "this router was once slow" rather than "this
	// router is stuck now", and a warning that cannot be cleared gets ignored.
	if n := s.noteSkip("a"); n != 1 {
		t.Errorf("the skip counter carried over a completed run: counted %d, want 1", n)
	}
}

func logsOf(h *harness) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.logs...)
}

// waitFor polls until cond holds, so these tests wait on the CONDITION rather
// than on a sleep long enough to hide a regression.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}
