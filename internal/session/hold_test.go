package session

import (
	"testing"
	"time"
)

// Phase 4.3a: a session may be kept alive for a reason that is not a viewer.
//
// ── WHY THE TWO KINDS OF CLAIM ARE SEPARATE ─────────────────────────────────
//
// `refs` counts viewers, and its zero has a meaning the grace timer depends on:
// the last browser left, start the countdown. A hold is a different claim —
// alerting, history recording, the Devices page — and it does not expire,
// because nobody is coming back to renew it.
//
// Folding them into one counter would make "held for alerting" indistinguishable
// from a viewer who never leaves, and the linger timer would never run for any
// router with alerting on. These tests pin that separation, because the failure
// it prevents is a session that is never torn down and nothing saying why.

func TestRetainNeedsAReason(t *testing.T) {
	m := &Manager{live: map[string]*Session{}}
	if _, err := m.Retain("r1", ""); err == nil {
		t.Error("an unnamed hold was accepted. Names are what make a stuck session " +
			"explainable and what stop a double release leaking one.")
	}
}

// TestHoldsAreIdempotentByName: the callers re-sync on every settings change and
// every router edit, so a hold taken twice must be one hold. Counting would leak.
func TestHoldsAreIdempotentByName(t *testing.T) {
	s := &Session{holds: map[string]bool{}}
	m := &Manager{live: map[string]*Session{"r1": s}}

	s.mu.Lock()
	s.holds["alerts"] = true
	s.holds["alerts"] = true
	s.holds["history"] = true
	s.mu.Unlock()

	got := m.Held("r1")
	if len(got) != 2 || got[0] != "alerts" || got[1] != "history" {
		t.Fatalf("Held = %v, want [alerts history] — sorted, so the reasons read the "+
			"same way every time somebody debugs a session that will not go away", got)
	}
	m.Drop("r1", "alerts")
	if got := m.Held("r1"); len(got) != 1 || got[0] != "history" {
		t.Errorf("after dropping alerts, Held = %v, want [history]", got)
	}
	// A second drop of the same reason is not an error and takes nothing else.
	m.Drop("r1", "alerts")
	if got := m.Held("r1"); len(got) != 1 {
		t.Errorf("a repeated Drop removed another hold: %v", got)
	}
}

// TestDroppingTheLastHoldStartsTheGraceRatherThanTearingDown.
//
// Not an immediate teardown, deliberately: a router that loses alerting and
// gains a viewer in the same sync would otherwise be destroyed and rebuilt,
// dropping its connection and every collector's history for no reason.
func TestDroppingTheLastHoldStartsTheGraceRatherThanTearingDown(t *testing.T) {
	s := &Session{holds: map[string]bool{"alerts": true}}
	m := &Manager{live: map[string]*Session{"r1": s}, idleGrace: time.Hour}

	m.Drop("r1", "alerts")

	s.mu.Lock()
	lingering := s.linger != nil
	s.mu.Unlock()
	if !lingering {
		t.Error("dropping the last hold did not start the grace timer, so the session " +
			"either dies at once or never")
	}
	if m.live["r1"] == nil {
		t.Error("the session was torn down immediately; a router that loses alerting " +
			"and gains a viewer in one sync would be rebuilt for nothing")
	}
}

// TestSpokenForIsWhatKeepsASessionAlive.
//
// ── WHY THE DECISION AND NOT THE TEARDOWN ───────────────────────────────────
//
// The first version of this test called Release and checked the session was
// still there. It PASSED against a manager that ignored holds entirely, because
// Release only arms a timer and the assertion ran long before it fired — a test
// that could not fail.
//
// Driving the real `idleOut` instead panicked: it dereferences a fully built
// session, and a hand-made one has no client, no scheduler and no history wire.
// So the check is `spokenForLocked`, which is the decision the teardown makes,
// and `TestIdleOutAsksSpokenFor` below is what stops that becoming a test of
// something nothing calls.
func TestSpokenForIsWhatKeepsASessionAlive(t *testing.T) {
	cases := []struct {
		name  string
		refs  int
		holds map[string]bool
		want  bool
	}{
		{"a viewer is watching", 1, nil, true},
		{"held for alerting with nobody watching", 0, map[string]bool{"alerts": true}, true},
		{"held for history with nobody watching", 0, map[string]bool{"history": true}, true},
		{"two holds", 0, map[string]bool{"alerts": true, "devices": true}, true},
		// The one that must be false, or nothing is ever torn down and every
		// router stays connected for ever.
		{"nobody at all", 0, map[string]bool{}, false},
		{"holds map never initialised", 0, nil, false},
	}
	for _, c := range cases {
		s := &Session{refs: c.refs, holds: c.holds}
		if got := s.spokenForLocked(); got != c.want {
			t.Errorf("%s: spokenFor = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestIdleOutAsksSpokenFor. The check above is only worth having while the
// teardown actually consults it; a refactor that inlined the old `refs > 0`
// would leave a green test guarding nothing.
func TestIdleOutAsksSpokenFor(t *testing.T) {
	src := readSource(t, "session.go")
	if !contains(src, "if s.spokenForLocked() {") {
		t.Error("idleOut no longer asks spokenForLocked, so a session held for alerting " +
			"is torn down when its last viewer leaves — silently, and that is the whole " +
			"gap the background pools exist to fill")
	}
}
