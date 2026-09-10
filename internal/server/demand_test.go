package server

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"mikrodash/internal/collect"
	"mikrodash/internal/session"
)

var reResume = regexp.MustCompile(`ResumeCollector\("(\w+)"\)`)

// roomlessCollectors are the collectors no room can ever want, with the reason.
//
// ── THE FAILURE THIS GUARDS IS SILENT AND TOTAL ────────────────────────────
//
// Phase 4.2b's rule is "a collector runs if anybody is in any room it declares".
// A collector that declares NONE is therefore never wanted by a viewer — it can
// only ever run for alerting or a hold, and on a router with neither it would be
// suspended from connect to teardown with nothing anywhere saying so.
//
// Under the switchboard that could not happen: a page's case called
// `ResumeCollector` by name whether or not the collector's rooms agreed. So this
// hazard is CREATED by the change, which is why it gets a gate rather than a
// comment.
//
// ── IT IS EMPTY, AND IT WAS NOT ─────────────────────────────────────────────
//
// `dhcpLeases` was the one entry, on the grounds that its payload reaches the
// browser only through the collectors that consume it. That was wrong in the way
// this gate exists to catch: it emits ROUTER-WIDE, so `RoomsOf` reports nothing
// for it while the DHCP page and the Connections page both render it directly.
// Suspending it would have blanked both. It is `keepAliveFor["dhcpLeases"]` now,
// which is why this map has no entries and why the question is asked of
// `DemandRooms` rather than `RoomsOf`.
//
// Keeping the map empty rather than deleting it is deliberate: the next
// collector with no audience needs somewhere to say so, and the gate below is
// what forces the reason to be written down instead of assumed.
var roomlessCollectors = map[string]string{}

// TestEveryGatedCollectorDeclaresARoom is the safety check the switchover needs.
func TestEveryGatedCollectorDeclaresARoom(t *testing.T) {
	var roomless []string
	for _, key := range session.TargetKeys() {
		if len(collect.DemandRooms(key)) == 0 {
			roomless = append(roomless, key)
		}
	}
	sort.Strings(roomless)

	for _, key := range roomless {
		if _, known := roomlessCollectors[key]; !known {
			t.Errorf("%s is gated by demand and no room can want it, so it runs only "+
				"for alerting or a hold: on a router with neither it is suspended from "+
				"connect to teardown with nothing saying so. Declare its rooms, add a "+
				"keepAliveFor entry, or record here why it needs neither.", key)
		}
	}
	// AND THE OTHER DIRECTION. A recorded exception that has GAINED rooms is a
	// note describing something that is no longer true.
	for key, why := range roomlessCollectors {
		if len(collect.DemandRooms(key)) > 0 {
			t.Errorf("%s is recorded as wanted by no room (%q) and now declares %v. "+
				"Drop the entry.", key, why, collect.DemandRooms(key))
		}
	}
}

// TestDemandCoversEveryCollectorTheSwitchboardDid.
//
// ── A MIGRATION CHECK THAT OUTLIVED ITS SUBJECT, DELIBERATELY ──────────────
//
// It was written before the switchover and read `ws.go` directly: scan the
// `ResumeCollector("…")` literals, and assert every collector the switchboard
// could start is one the demand rule can also start. Converting a page must not
// silently drop a collector.
//
// The switchboard is gone, so the scan now finds nothing and the check would
// pass by reading an empty set — the failure mode this repository has lost
// coverage to before. The list is therefore FROZEN here, as it stood at
// 8ffc8a4, and the assertion is unchanged: every collector the switchboard could
// start must still be gatable by demand today.
//
// That keeps meaning something after the deletion. A collector that loses its
// entry from `session.TargetKeys()` — or is renamed without the table following
// — was reachable by the shipped app and is no longer reachable by anything, and
// this is what says so.
var switchboardResumed = []string{
	"bandwidth", "bridges", "capsman", "conns", "dhcpLeases", "dhcpNetworks",
	"dns", "firewall", "packages", "ppp", "queues", "rosusers", "routing",
	"topology", "vlans", "vpn", "wan", "wifi", "wireless",
}

func TestDemandCoversEveryCollectorTheSwitchboardDid(t *testing.T) {
	// The frozen list is only as good as its provenance, and a typo in it would
	// weaken the check silently. Nineteen is what the switchboard resumed, and it
	// is the number the pre-switchover run of this test measured.
	if len(switchboardResumed) != 19 {
		t.Fatalf("the frozen switchboard list holds %d keys; it held 19 when it was "+
			"taken, so it has been edited without its note being updated",
			len(switchboardResumed))
	}

	gated := map[string]bool{}
	for _, k := range session.TargetKeys() {
		gated[k] = true
	}
	for _, key := range switchboardResumed {
		if !gated[key] {
			t.Errorf("the shipped switchboard resumed %q and the target table cannot "+
				"gate it, so nothing can start that collector any more", key)
		}
	}

	resumed := map[string]bool{}
	for _, k := range switchboardResumed {
		resumed[k] = true
	}
	var newlyGated []string
	for _, k := range session.TargetKeys() {
		if !resumed[k] {
			newlyGated = append(newlyGated, k)
		}
	}
	sort.Strings(newlyGated)
	// The three the switchboard NEVER resumed are the interesting direction:
	// they were never gated by it at all, and ran from connect until dormancy or
	// a hold pruned them. Under demand they are gated like everything else, which
	// is the behaviour CHANGE of this phase.
	//
	// `ifStatus` is the one that needed work rather than a note: four collectors
	// borrow its rates on pages it emits nothing to, so it is gated on
	// `keepAliveFor` as well as its own audience. `netwatch` and `talkers` feed
	// only the dashboard, and now stop when nobody is on it.
	want := "ifStatus,netwatch,talkers"
	if got := strings.Join(newlyGated, ","); got != want {
		t.Errorf("collectors newly gated by demand = %q, want %q.\nIf that list has "+
			"changed, a collector has gained or lost coverage and the behaviour change "+
			"of this phase is different from the one recorded.", got, want)
	}
}

// TestNoSwitchboardHasComeBack: the source half, which the frozen list above
// cannot provide.
//
// A `ResumeCollector("…")` literal in a page handler is the switchboard
// returning one case at a time. That is not a hypothetical: the shape is
// obvious, it compiles, and it works — right up until the collector gains a
// second room and the handler that starts it is not the one that would stop it.
func TestNoSwitchboardHasComeBack(t *testing.T) {
	b, err := os.ReadFile("ws.go")
	if err != nil {
		t.Fatal(err)
	}
	// Comments first: ws.go describes at length what was removed.
	var body strings.Builder
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		body.WriteString(line)
		body.WriteByte('\n')
	}
	for _, m := range reResume.FindAllStringSubmatch(body.String(), -1) {
		t.Errorf("ws.go resumes %q by name. A handler joins its room; applyDemand "+
			"decides what runs. Naming a collector here puts the mapping back in two "+
			"places, which is what phase 4.2b removed.", m[1])
	}
}
