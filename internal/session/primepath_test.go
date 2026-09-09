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

	for key := range feedsAPage {
		why, exempt := noPrimePath[key]
		switch {
		case inTargets[key] && exempt:
			t.Errorf("%s is recorded as having no prime path (%q) and IS in targetKeys, "+
				"so `primeAll` primes it. The exemption has closed — delete it.", key, why)
		case !inTargets[key] && !exempt:
			t.Errorf("%s emits to a page and is not in targetKeys, so `primeAll` never "+
				"reaches it: landing on that page waits out the collector's whole first "+
				"cadence, which for some is ten minutes. Add it to targets() and "+
				"targetKeys, or record here why it cannot have a prime path.", key)
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
func TestEveryPrimeTargetCanActuallyRefresh(t *testing.T) {
	src := readSource(t, "dormancy_targets.go")
	for _, k := range targetKeys {
		// Each target is registered as `add("key", ...)` with the refresh as its
		// last argument. A nil there is the silent case.
		if strings.Contains(src, `add("`+k+`",`) && strings.Contains(src, `add("`+k+`", nil`) {
			t.Errorf("%s is registered with a nil accessor", k)
		}
	}
	if n := strings.Count(src, ", nil)"); n > 0 {
		t.Errorf("%d target(s) are registered with a nil refresh. `primeAll` skips those "+
			"without a word, so the collector is in the list and still has no prime "+
			"path — the exact gap this pair of tests exists to close.", n)
	}
}
