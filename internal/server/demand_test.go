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

// roomlessCollectors are the collectors that declare no room, with the reason.
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
var roomlessCollectors = map[string]string{
	"dhcpLeases": "feeds no room of its own: its payload reaches the browser only " +
		"through the collectors that consume it (conns, wireless, topology, " +
		"bandwidth). It is wanted when THEY are, and the holds cover the rest — " +
		"it is in devicesFeeds",
}

// TestEveryGatedCollectorDeclaresARoom is the safety check the switchover needs.
func TestEveryGatedCollectorDeclaresARoom(t *testing.T) {
	var roomless []string
	for _, key := range session.TargetKeys() {
		if len(collect.RoomsOf(key)) == 0 {
			roomless = append(roomless, key)
		}
	}
	sort.Strings(roomless)

	for _, key := range roomless {
		if _, known := roomlessCollectors[key]; !known {
			t.Errorf("%s is gated by demand and declares NO ROOM, so no viewer can "+
				"ever want it: it runs only for alerting or a hold, and on a router "+
				"with neither it is suspended from connect to teardown with nothing "+
				"saying so. Declare its rooms, or record here why it has none.", key)
		}
	}
	// AND THE OTHER DIRECTION. A recorded exception that has GAINED rooms is a
	// note describing something that is no longer true.
	for key, why := range roomlessCollectors {
		if len(collect.RoomsOf(key)) > 0 {
			t.Errorf("%s is recorded as declaring no room (%q) and now declares %v. "+
				"Drop the entry.", key, why, collect.RoomsOf(key))
		}
	}
}

// TestDemandCoversEveryCollectorTheSwitchboardDid.
//
// ── THE AGREEMENT CHECK, RUN BEFORE ANYTHING IS DELETED ────────────────────
//
// The switchboard resumes 19 collectors by name across `ws.go`. The new rule
// asks its question of all 22 in the target table. Every collector the
// switchboard could start must be one the new rule can also start — otherwise
// converting a page silently drops a collector.
//
// The three the switchboard NEVER resumes — `ifStatus`, `netwatch`, `talkers` —
// are the interesting direction: they were never gated by it at all. They ran
// from connect until dormancy or a hold pruned them. Under the new rule they are
// gated like everything else, which is a behaviour CHANGE and the reason it is
// named here rather than discovered later.
func TestDemandCoversEveryCollectorTheSwitchboardDid(t *testing.T) {
	b, err := os.ReadFile("ws.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	resumed := map[string]bool{}
	for _, m := range reResume.FindAllStringSubmatch(src, -1) {
		resumed[m[1]] = true
	}
	if len(resumed) < 15 {
		t.Fatalf("found only %d ResumeCollector call(s) in ws.go; the switchboard "+
			"scan has stopped matching and this check is measuring nothing", len(resumed))
	}

	gated := map[string]bool{}
	for _, k := range session.TargetKeys() {
		gated[k] = true
	}
	for key := range resumed {
		if !gated[key] {
			t.Errorf("the switchboard resumes %q and the target table cannot gate it, "+
				"so converting its page would drop the collector entirely", key)
		}
	}

	var newlyGated []string
	for _, k := range session.TargetKeys() {
		if !resumed[k] {
			newlyGated = append(newlyGated, k)
		}
	}
	sort.Strings(newlyGated)
	want := "ifStatus,netwatch,talkers"
	if got := strings.Join(newlyGated, ","); got != want {
		t.Errorf("collectors newly gated by demand = %q, want %q.\nIf that list has "+
			"changed, a collector has gained or lost switchboard coverage and the "+
			"behaviour change of this phase is different from the one recorded.",
			got, want)
	}
}
