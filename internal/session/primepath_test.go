package session

import (
	"strings"
	"testing"

	"mikrodash/internal/collect"
	"mikrodash/internal/collection"
)

// noPrimePath records the collectors that feed a page and deliberately have no
// prime path, with the reason each one cannot have one.
//
// Both are set B: an acquisition that produces a SEQUENCE rather than a table,
// so "take one reading" is not a thing that can be asked of them.
//
// ── IT IS A NIL `refresh`, NOT AN ABSENCE FROM `targetKeys` (2026-09-10) ───
//
// These two questions were the same list until 3.4, and conflating them was
// costing something real: `logs` and `ping` were kept OUT of `targetKeys`
// partly because being in it would have made `primeAll` prime them — and being
// out of it meant `applyDemand` could not gate them either, so both ran from
// connect to teardown, `logs` holding a channel per router for a page nobody
// had open.
//
// They are separate properties and the table already carried both: membership
// says demand can reach a collector, and a non-nil `refresh` says one reading
// can be asked of it. `primeAll` has always skipped a nil `refresh`; what was
// missing is that the skip was silent. This ledger is what makes it spoken.
var noPrimePath = map[string]string{
	"logs": "/log/listen is a push channel with no readable state — there is no " +
		"'current value' to fetch. The Logs page starts empty on any implementation " +
		"and fills as the router writes, which is what a log IS",
	"ping": "a ping is a measurement that takes as long as it takes; priming it " +
		"would mean waiting for a round trip on the connect path. The dashboard's " +
		"ping card fills on the first result instead",
}

// TestEveryPageFeedingCollectorHasAPrimePath is 5.3.
//
// ── THE HANG THIS PREVENTS, AND WHY NOTHING ELSE CATCHES IT ────────────────
//
// The operator's requirement is that landing on a page never waits for data.
// `Last()` replay serves any collector that has produced, so the hang is
// specifically the FIRST landing, before a collector's own cadence has come
// round — and some of those cadences are ten minutes.
//
// `primeAll` closes it by asking every collector that has never produced for one
// reading. It walks `targetKeys`.
//
// SO A COLLECTOR ABSENT FROM `targetKeys` HAS NO PRIME PATH, and the existing
// drift gate does not catch that: `TestTheTableCoversEveryEligibleCollector`
// checks the table against `collection.DormancyEligible()`, and eligibility
// comes from having an `emptyKey`. A new collector with no `emptyKey` is
// eligible for nothing, required by no list, and its page waits out its whole
// first cadence with nothing anywhere saying so.
//
// This asks the other question: not "can dormancy judge it" but "does anybody
// looking at its page have to wait".
//
// ── IT FAILS IN BOTH DIRECTIONS ────────────────────────────────────────────
//
// An unlisted page-feeding collector fails. So does an EXEMPTION THAT HAS
// CLOSED: if `logs` or `ping` ever gains a prime path, this fails and the
// exemption must be deleted rather than left as folklore. That is the rule this
// repository applies to every ledger, and the one that keeps them worth reading.
func TestEveryPageFeedingCollectorHasAPrimePath(t *testing.T) {
	inTargets := map[string]bool{}
	for _, k := range targetKeys {
		inTargets[k] = true
	}

	feedsAPage := map[string]bool{}
	for _, c := range collection.Collectors() {
		for _, room := range collect.RoomsOf(c.Key) {
			if strings.HasPrefix(room, "page-") {
				feedsAPage[c.Key] = true
				break
			}
		}
	}
	// A BELIEVABILITY FLOOR. If the room declarations stop resolving, every
	// assertion below passes over an empty set.
	if len(feedsAPage) < 10 {
		t.Fatalf("only %d collector(s) appear to feed a page; the room declarations "+
			"have stopped resolving and this check is measuring nothing", len(feedsAPage))
	}

	refreshable := targetsWithARefresh(t)
	for key := range feedsAPage {
		why, exempt := noPrimePath[key]
		switch {
		case refreshable[key] && exempt:
			t.Errorf("%s is recorded as having no prime path (%q) and now registers a "+
				"refresh, so `primeAll` primes it. The exemption has closed — delete it.",
				key, why)
		case !refreshable[key] && !exempt:
			if !inTargets[key] {
				t.Errorf("%s emits to a page and is not in targetKeys at all, so nothing "+
					"can gate it and `primeAll` never reaches it: it runs from connect to "+
					"teardown, and landing on its page waits out its whole first cadence. "+
					"Add it to targets() and targetKeys.", key)
				continue
			}
			t.Errorf("%s emits to a page and registers no refresh, so `primeAll` skips "+
				"it: landing on that page waits out the collector's whole first cadence, "+
				"which for some is ten minutes. Give it a refresh, or record here why it "+
				"cannot have one.", key)
		}
	}

	// And an exemption for a collector that no longer feeds a page at all is a
	// note about nothing.
	for key := range noPrimePath {
		if !feedsAPage[key] {
			t.Errorf("noPrimePath records %s, which no longer emits to any page. Drop "+
				"the entry.", key)
		}
	}
}

// TestEveryPrimeTargetCanActuallyRefresh. Being in `targetKeys` is not the same
// as being primeable: `primeAll` skips a target whose `refresh` is nil, silently.
// A collector added to the table without one looks covered by the check above and
// is not.
//
// A nil refresh is ALLOWED, and only when `noPrimePath` says why. That is the
// ledger rule this repository applies everywhere: the gap is documented, never
// hidden, and it fails in both directions.
func TestEveryPrimeTargetCanActuallyRefresh(t *testing.T) {
	refreshable := targetsWithARefresh(t)
	for _, k := range targetKeys {
		if refreshable[k] {
			continue
		}
		if _, exempt := noPrimePath[k]; !exempt {
			t.Errorf("%s is registered with a nil refresh. `primeAll` skips those "+
				"without a word, so the collector is in the list and still has no prime "+
				"path — the exact gap this pair of tests exists to close. Give it a "+
				"refresh, or record the reason in noPrimePath.", k)
		}
	}
	// AND THE OTHER DIRECTION: an exemption for a key the table does not hold is
	// a note about nothing, and would hide the next collector to take that name.
	for k := range noPrimePath {
		if !inTargetTable(k) {
			t.Errorf("noPrimePath records %s, which is not in targetKeys. The entry "+
				"exempts nothing.", k)
		}
	}
}

// targetsWithARefresh reads which targets register a non-nil refresh.
//
// SOURCE-READ, like everything else in this file and for the same reason:
// building a Session needs a router. Each target is one `add("key", …)` call
// whose LAST argument is the refresh, so a registration ending `, nil)` is the
// silent case and anything else is a real closure.
func targetsWithARefresh(t *testing.T) map[string]bool {
	t.Helper()
	src := readSource(t, "dormancy_targets.go")
	out := map[string]bool{}
	chunks := strings.Split(src, `add("`)
	for _, c := range chunks[1:] {
		key, rest, ok := strings.Cut(c, `"`)
		if !ok {
			continue
		}
		// ── THE SOURCE ARRIVES FLATTENED, WHICH COST A DEBUGGING ROUND ──────
		//
		// `readSource` strips comments and collapses every run of whitespace to
		// one space, so there are NO NEWLINES to cut a registration at. A first
		// version looked for `)\n` and found none: the fallback then took each
		// chunk whole, which is right for every entry except the LAST, whose
		// chunk runs to the end of the file. `ping` alone came back wrong, and
		// wrong in the direction that reads as "this has a refresh".
		//
		// Splitting on `add("` already ends every other chunk at the next
		// registration; the last one needs the function's own close.
		if i := strings.Index(rest, "return t }"); i >= 0 {
			rest = rest[:i]
		}
		// The refresh is the LAST argument, so a registration ending `, nil)` is
		// the silent case and anything else is a real closure.
		out[key] = !strings.HasSuffix(strings.TrimSpace(rest), ", nil)")
	}
	if len(out) != len(targetKeys) {
		t.Fatalf("read %d add() registrations for %d target keys — targets() and "+
			"targetKeys have drifted, and this helper is reading the wrong thing",
			len(out), len(targetKeys))
	}
	return out
}
