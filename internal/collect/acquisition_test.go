package collect

import "testing"

// TestMenuAvailableTreatsNilAsNotYetKnown.
//
// ── THE HALF A READER GETS BACKWARDS ───────────────────────────────────────
//
// Ten call sites across eight collectors spelled this rule out by hand before it
// had one home. It is a THREE-state — nil is "nothing has answered yet", not
// "absent" — and nil must read as AVAILABLE.
//
// Getting it the other way round blanks every page on the first tick, before
// anything has come back, which looks exactly like a router that lacks the
// feature. That asymmetry is the whole reason the field is a pointer.
func TestMenuAvailableTreatsNilAsNotYetKnown(t *testing.T) {
	yes, no := true, false
	if !MenuAvailable(nil) {
		t.Error("nil read as unavailable — every page blanks on the first tick, " +
			"before the router has answered, and looks like a missing feature")
	}
	if !MenuAvailable(&yes) {
		t.Error("an explicit yes read as unavailable")
	}
	if MenuAvailable(&no) {
		t.Error("an explicit no did not survive — a router that answered and does " +
			"not have the menu would be shown an empty table instead of being told")
	}
}
