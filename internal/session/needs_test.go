package session

import (
	"strings"
	"testing"
)

// The decision that keeps 4.3 from being a regression.
//
// Replacing the background pools with sessions is a LOSS unless a session held
// for alerting runs only what alerting needs: the pool costs 119-120 commands a
// minute for one unwatched router, and a session's connect block starts fifteen
// collectors against the pool's seven.
func TestNeedsGivesAnAlertingRouterOnlyTheAlertFeed(t *testing.T) {
	why := Reasons{Alerts: true}

	for _, k := range AlertFeeds {
		if !Needs(k, why) {
			t.Errorf("%s feeds an alert rule and would not run on a router held for "+
				"alerting — the rule then never fires, silently", k)
		}
	}
	// The nine that make the naive version a regression.
	for _, k := range []string{"bridges", "dhcpLeases", "dhcpNetworks", "dns",
		"firewall", "logs", "talkers", "vlans", "wan"} {
		if Needs(k, why) {
			t.Errorf("%s would run on a router held only for alerting. Nobody is "+
				"watching it and no rule reads it; `wan` alone polls every two seconds.", k)
		}
	}
}

// TestAViewerWantsEverything, because any page can be navigated to and the page
// gates decide the rest. This is existing behaviour and the merge must not
// narrow it.
func TestAViewerWantsEverything(t *testing.T) {
	why := Reasons{Viewer: true}
	for _, k := range []string{"bridges", "wan", "vlans", "queues", "capsman", "logs"} {
		if !Needs(k, why) {
			t.Errorf("a viewer would not get %s, so navigating to its page shows nothing", k)
		}
	}
}

// TestReasonsCombine: a router can be watched AND alerting AND recorded, and the
// answer is the union. A miss here starves whichever consumer was not counted.
func TestReasonsCombine(t *testing.T) {
	if !Needs("traffic", Reasons{History: true}) {
		t.Error("history does not get traffic, so traffic_samples stops being written")
	}
	if !Needs("ping", Reasons{History: true}) {
		t.Error("history does not get ping, so ping_samples stops being written")
	}
	if !Needs("vpn", Reasons{Alerts: true, History: true}) {
		t.Error("the union of two reasons lost a collector one of them needs")
	}
	if Needs("wan", Reasons{Alerts: true, History: true, Devices: true}) {
		t.Error("wan runs for a router with no viewer; none of those three reads it")
	}
}

// TestNothingRunsForNoReason. Without this the check above passes against a
// Needs that simply returns true.
func TestNothingRunsForNoReason(t *testing.T) {
	for _, k := range []string{"system", "ping", "traffic", "vpn", "wan"} {
		if Needs(k, Reasons{}) {
			t.Errorf("%s runs for a session nobody holds and nobody views", k)
		}
	}
}

// TestApplyReasonsLeavesAViewerAlone.
//
// `Needs` says a viewer wants everything, which is about what is ALLOWED to run,
// not what should be running now. Page gating decides that, and it is the reason
// an idle browser does not poll twenty-two collectors. Resuming everything
// because a viewer exists would undo all of it — so the prescriptive case is the
// viewerless one, and this pins that.
func TestApplyReasonsLeavesAViewerAlone(t *testing.T) {
	src := readSource(t, "needs.go")
	if !contains(src, "if why.Viewer { return }") {
		t.Error("applyReasons no longer returns early for a viewer, so it would resume " +
			"every collector for any browser and undo page gating entirely")
	}
	if !contains(src, "if s == nil || !s.Connected() { return }") {
		t.Error("applyReasons no longer guards on Connected. A hold taken while the " +
			"session is dialling then reaches collectors that do not exist yet.")
	}
	if !contains(src, "s.ResumeCollector(key)") {
		t.Error("applyReasons resumes a collector without the funnel, so a collector the " +
			"operator disabled for this router comes back because alerting wants it")
	}
}

// TestEveryTransitionConverges: the order a hold and a connect arrive in is
// racy, so every one of them must re-apply. A missing call leaves a session
// running the wrong set until something else happens to touch it.
func TestEveryTransitionConverges(t *testing.T) {
	src := readSource(t, "session.go")
	for _, where := range []string{
		// The Retain path. NOT "hold then applyReasons" -- that adjacency is what
		// the first version of this test asserted, and it was pinning the BUG:
		// applyReasons no-ops while a viewer is present, and Acquire's reference
		// is still held at that point. The correct shape is release, THEN apply,
		// and TestRetainPrunesAfterGivingBackItsViewerReference checks the order
		// directly. Here it is enough that the Retain path applies at all.
		"m.Release(routerID)",
		// The Drop path. Checked as the whole sequence, because `delete(s.holds,
		// reason)` alone is trivially present and a mutation removing the
		// applyReasons after it SURVIVED the first version of this test.
		"delete(s.holds, reason) empty := len(s.holds) == 0 && s.refs <= 0 s.mu.Unlock() s.applyReasons()",
		// The connect path. NOT deferred -- see TestTheConnectPruneIsNotDeferred.
		"s.applyReasons() first = false",
	} {
		if !contains(src, where) {
			t.Errorf("a holder transition no longer calls applyReasons (%q). The session "+
				"then runs whatever the last transition left, which for an alerting "+
				"router is fifteen collectors instead of six.", where)
		}
	}
}

// TestRetainPrunesAfterGivingBackItsViewerReference.
//
// ── THE BUG THIS PINS, WHICH EVERY TEST MISSED ──────────────────────────────
//
// `Retain` acquires (taking a VIEWER reference), records the hold, and releases.
// `applyReasons` does nothing while a viewer is present — so calling it between
// the hold and the release saw refs == 1, concluded a viewer wanted everything,
// and returned. The session then ran all fifteen collectors for a router nobody
// was watching.
//
// Every test was green. The tests ask what the code DECIDES; none of them could
// see how much the router was being asked. It was found by measuring: 119-120
// commands a minute became 263-311.
func TestRetainPrunesAfterGivingBackItsViewerReference(t *testing.T) {
	src := readSource(t, "session.go")
	rel := indexOf(src, "m.Release(routerID) ")
	app := indexOf(src, "s.applyReasons() return s, nil")
	if rel < 0 || app < 0 {
		t.Fatal("Retain no longer releases then applies; this check is reading the wrong " +
			"shape and would pass against anything")
	}
	if app < rel {
		t.Error("Retain calls applyReasons BEFORE giving back its viewer reference. " +
			"applyReasons no-ops while a viewer is present, so the prune never happens " +
			"and a router held for alerting runs every collector.")
	}
}

// TestAViewerLeavingAHeldSessionPrunes. Without it, one browser visit
// permanently upgrades an alerting router to the full viewer set.
func TestAViewerLeavingAHeldSessionPrunes(t *testing.T) {
	src := readSource(t, "session.go")
	if !contains(src, "held := len(s.holds) > 0") || !contains(src, "if held { s.applyReasons() }") {
		t.Error("Release no longer prunes a held session when its last viewer leaves, so " +
			"visiting a router once leaves it running the viewer's collector set for ever")
	}
}

// indexOf is strings.Index over the flattened source the two checks above read.
func indexOf(src, want string) int {
	return strings.Index(src, strings.Join(strings.Fields(want), " "))
}

// TestTheConnectPruneIsNotDeferred.
//
// It was written as `defer s.applyReasons()`. A defer runs when the FUNCTION
// returns, and the function is `connectLoop` — a loop that runs for the life of
// the session and never returns. So the prune never happened, and a router held
// only for alerting kept all fifteen collectors.
//
// Every test stayed green, because they assert the call EXISTS and it did. It
// took a command-rate measurement to find: 264-287 a minute against a 119-120
// baseline. This is the cheap version of that measurement.
func TestTheConnectPruneIsNotDeferred(t *testing.T) {
	src := readSource(t, "session.go")
	if contains(src, "defer s.applyReasons()") {
		t.Error("the connect path defers applyReasons. connectLoop never returns, so a " +
			"deferred call never runs and a held session keeps every collector.")
	}
	if !contains(src, "s.replayResumes() // ── PHASE 4.3c") && !contains(src, "s.applyReasons() first = false") {
		t.Error("the connect path no longer prunes at the end of the start block, so what " +
			"a held session runs depends on whatever touched it last")
	}
}

// TestResumeCollectorRefusesWhatTheSessionHasNoReasonToRun.
//
// The connect-time prune is not enough on its own. The dormancy probe calls
// `ResumeCollector` for any collector due for a probe, and it has no way to know
// why the session exists — so after the prune it talked `queues` back into
// running on a router nobody was watching.
//
// Found by measurement, not by reading: the rate settled at 147-167 a minute
// against a 119-120 baseline, and the busiest-menu list named `/queue/simple`
// and `/queue/tree`. The funnel is where the veto belongs, beside the enabled
// check, because every resume in the app goes through it.
func TestResumeCollectorRefusesWhatTheSessionHasNoReasonToRun(t *testing.T) {
	src := readSource(t, "dormancy_targets.go")
	if !contains(src, "why := s.reasonsLocked()") || !contains(src, "if !Needs(key, why) { return }") {
		t.Error("ResumeCollector no longer refuses a collector the session has no reason " +
			"to run, so the dormancy probe resumes whatever it likes on a held session " +
			"and the prune is undone within a minute")
	}
	// Beside the enabled check, not after the work: a refusal that happens later
	// has already started something.
	if indexOf(src, "if !Needs(key, why) { return }") > indexOf(src, "if !s.Connected() {") {
		t.Error("the Needs veto is after the not-connected latch, so a refused resume is " +
			"still remembered and replayed when the link comes up")
	}
}
