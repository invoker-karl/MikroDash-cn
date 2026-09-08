package session

import "testing"

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
		"s.holds[reason] = true s.mu.Unlock() s.applyReasons()",
		// The Drop path. Checked as the whole sequence, because `delete(s.holds,
		// reason)` alone is trivially present and a mutation removing the
		// applyReasons after it SURVIVED the first version of this test.
		"delete(s.holds, reason) empty := len(s.holds) == 0 && s.refs <= 0 s.mu.Unlock() s.applyReasons()",
		"defer s.applyReasons()",
	} {
		if !contains(src, where) {
			t.Errorf("a holder transition no longer calls applyReasons (%q). The session "+
				"then runs whatever the last transition left, which for an alerting "+
				"router is fifteen collectors instead of six.", where)
		}
	}
}
