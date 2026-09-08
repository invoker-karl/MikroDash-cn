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
