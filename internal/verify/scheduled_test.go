package verify

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestScheduledCollectorsAreDeclared records which collectors have moved off
// their own timers onto the router's scheduler, and — more usefully — which have
// not and why.
//
// ── WHY THIS IS A GATE AND NOT A PARAGRAPH ──────────────────────────────────
//
// Phase 3.3 of Collectors-Rewrite.md proposes replacing the dormancy supervisor
// with per-query backoff. It is BLOCKED, and blocked on a number: only 8 of the
// 18 dormancy-eligible collectors are scheduled. Per-query backoff would cover
// those eight while the supervisor kept ticking for the other ten, leaving two
// dormancy mechanisms with different semantics running side by side.
//
// That number is the whole argument, and a number in a document is exactly the
// kind of thing this repository has watched go stale. Here it re-measures itself.
// **When `unscheduled` empties, 3.3 becomes worth revisiting** — and the test
// says so rather than leaving somebody to notice.
//
// It fails in both directions: a collector that starts subscribing without being
// listed fails, and a listed one that stops fails too.
//
// `Unscheduled_Collectors.md` is the readable companion to the `unscheduled` map
// below -- same twelve entries, same grades, plus what would have to change for
// each. It is gitignored, like the other working documents, so THIS is the copy
// that survives a clone: keep the reasons here even when the prose moves.
func TestScheduledCollectorsAreDeclared(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "internal", "collect")

	// file -> registry key. Explicit, because the two names differ often enough
	// (conns/connections, rosusers/rosUsers) that deriving it would be a second
	// source of truth.
	scheduled := map[string]string{
		"netwatch.go":     "netwatch",
		"talkers.go":      "talkers",
		"dns.go":          "dns",
		"packages.go":     "packages",
		"rosusers.go":     "rosusers",
		"queues.go":       "queues",
		"bridges.go":      "bridges",
		"system.go":       "system",
		"connections.go":  "conns",
		"dhcpleases.go":   "dhcpLeases",
		"dhcpnetworks.go": "dhcpNetworks",
		"wan.go":          "wan",
		"capsman.go":      "capsman",
		"ppp.go":          "ppp",
		"routing.go":      "routing",
		"ifstatus.go":     "ifStatus",
		"topology.go":     "topology",
		"vpn.go":          "vpn",
	}

	// ── EVERY UNSCHEDULED COLLECTOR, AND HOW STRONG THE REASON ACTUALLY IS ──
	//
	// Kept complete rather than limited to the dormancy-eligible ones, and graded
	// on purpose. "Cannot" and "have not" are different claims, and three of the
	// four obstacles recorded under 3.2 turn out to be the second kind when
	// written down honestly. The grade is the thing to challenge.
	//
	//	IMPOSSIBLE   there is no cadence to schedule. A stream has no result and
	//	             no end; scheduling it is not a thing that means anything.
	//	NEEDS 4.2    driven by another collector's OUTPUT. Wants a subscription to
	//	             a derivation, which does not exist yet.
	//	PARTIAL      part of it is schedulable today and has not been done. These
	//	             are the weakest claims on this list.
	//	NOT TRIED    no obstacle found. Just not done.
	unscheduled := map[string]string{
		// IMPOSSIBLE.
		"logs":    "IMPOSSIBLE — /log/listen is a push channel: no result to key, no end to key it until",
		"ping":    "IMPOSSIBLE — a stream parameterised by address; two consumers are asking about different hosts",
		"traffic": "IMPOSSIBLE — a monitor stream; same menu as ifStatus's measurement, a different question",

		// NEEDS 4.2.
		"bandwidth": "NEEDS 4.2 — takes the PARSED snapshot from connTable. Scheduling it on the " +
			"connection menu would duplicate the heaviest read in the app, which connTable exists to avoid",
		"vlans": "NEEDS 4.2 — reads only config menus; the reason it ticks at all is rates borrowed " +
			"from ifStatus in memory. Subscribing it to a menu would slow its rate column from 5s to 60s",

		// PARTIAL — the weakest claims here, and the ones worth arguing about.
		"firewall": "PARTIAL — pollCounters reads whichever table the operator has open, so the menu is " +
			"chosen at runtime. Re-subscribing when activeTable changes is possible and was not tried",
		"wifi": "PARTIAL — latches modern or legacy after probing, so its menu is not known at " +
			"construction. Re-subscribing when the latch flips is possible and was not tried",
		"wireless": "PARTIAL — same latch as wifi",
	}

	// A collector is on the scheduler when its file embeds the helper.
	marker := "sched scheduled"
	for _, name := range collectGoFiles(t, dir) {
		src := mustRead(t, filepath.Join(dir, name))
		has := strings.Contains(strings.Join(strings.Fields(src), " "), marker)
		_, listed := scheduled[name]
		switch {
		case has && !listed:
			t.Errorf("%s subscribes to a menu and is not in the scheduled list. Add it, and "+
				"check whether it closes one of the 3.3 blockers.", name)
		case !has && listed:
			t.Errorf("%s is listed as scheduled and no longer embeds the helper. A recorded "+
				"state that has changed is a failure here.", name)
		}
	}

	// ── THE 3.3 ARITHMETIC, RE-MEASURED ─────────────────────────────────────
	var tables struct {
		Registry []struct {
			Key         string   `json:"key"`
			EmptyKey    []string `json:"emptyKey"`
			Disableable bool     `json:"disableable"`
		} `json:"registry"`
	}
	raw := mustRead(t, filepath.Join(root, "internal", "collection", "collection_tables.json"))
	if err := json.Unmarshal([]byte(raw), &tables); err != nil {
		t.Fatalf("collection_tables.json: %v", err)
	}

	onScheduler := map[string]bool{}
	for _, key := range scheduled {
		onScheduler[key] = true
	}

	var eligible, covered, missing []string
	for _, c := range tables.Registry {
		if len(c.EmptyKey) == 0 || !c.Disableable {
			continue
		}
		eligible = append(eligible, c.Key)
		if onScheduler[c.Key] {
			covered = append(covered, c.Key)
			continue
		}
		missing = append(missing, c.Key)
		if unscheduled[c.Key] == "" {
			t.Errorf("%s is dormancy-eligible, not scheduled, and has no recorded reason. "+
				"Either schedule it or say what stops it — 3.3 is blocked on exactly this "+
				"list and an unexplained entry makes the blockage unreadable.", c.Key)
		}
	}
	for key := range unscheduled {
		if onScheduler[key] {
			t.Errorf("%s is listed as unscheduled with reason %q, but it IS on the scheduler "+
				"now. Drop the entry — and if the list is empty, 3.3 is unblocked.",
				key, unscheduled[key])
		}
	}

	sort.Strings(missing)
	if len(missing) == 0 {
		t.Errorf("every dormancy-eligible collector is scheduled. That is not a failure in the "+
			"code — it means PHASE 3.3 IS UNBLOCKED and this assertion has done its job. "+
			"Revisit per-query backoff, then delete this check. (%d eligible, all covered)",
			len(eligible))
	}
	t.Logf("%d dormancy-eligible collectors, %d scheduled, %d not: %v",
		len(eligible), len(covered), len(missing), missing)
}
