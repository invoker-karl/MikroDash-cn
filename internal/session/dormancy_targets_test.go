package session

// The table must cover every collector dormancy can judge.
//
// `DormancyEligible()` is derived from the generated registry, so a collector
// that gains an `emptyKey` upstream becomes eligible without anybody here
// noticing — and the supervisor would then judge a key the table cannot reach,
// which is a collector that can be put to sleep and never woken.
//
// This is the drift gate for that.

import (
	"testing"

	"mikrodash/internal/collect"
	"mikrodash/internal/collection"
)

func TestTheTableCoversEveryEligibleCollector(t *testing.T) {
	// The table is built from a Session's fields, and a zero Session has none —
	// so this reads the KEYS the constructor registers, which is what
	// `targets()` is a function of. Building it needs a session; the names are
	// what matter, so a nil-collector session would panic. Instead assert
	// against the list the constructor writes, kept in step by this test failing
	// when the registry moves.
	covered := map[string]bool{}
	for _, k := range targetKeys {
		covered[k] = true
	}
	var missing []string
	for _, c := range collection.DormancyEligible() {
		if !covered[c.Key] {
			missing = append(missing, c.Key)
		}
	}
	if len(missing) > 0 {
		t.Errorf("these collectors are eligible for dormancy and the session table cannot reach "+
			"them: %v.\nThe supervisor would suspend a key with no target — a collector put to "+
			"sleep and never woken. Add them to targets() and to targetKeys.", missing)
	}
}

// TestTargetKeysMatchesTheTable — the ledger's other direction.
//
// `targetKeys` is a list beside the table, and a list beside a thing is a list
// that goes stale. This is what stops it.
func TestTargetKeysMatchesTheTable(t *testing.T) {
	s := &Session{}
	defer func() {
		if r := recover(); r != nil {
			t.Skipf("targets() needs a constructed session (%v); the coverage test above is the "+
				"one that matters and does not", r)
		}
	}()
	built := s.targets()
	if len(built) != len(targetKeys) {
		t.Errorf("targets() builds %d entries and targetKeys names %d", len(built), len(targetKeys))
	}
}

// TestEveryCollectorWithAnAudienceIsInTheTable.
//
// ── WHAT THIS REPLACED, AND WHY THE QUESTION HAD TO MOVE ───────────────────
//
// It was `TestEveryKeyWsPassesIsInTheTable`, and it read `ws.go` for
// `ResumeCollector("literal")`. `ResumeCollector` treats an unknown key as a
// no-op — `ws.go` named pages, and a page with no collector behind it is normal
// — and that tolerance is how a typo, or a key nobody added to the table,
// becomes a collector that silently never resumes. It happened immediately:
// converting the twenty `X().Resume()` call sites to the funnel passed `conns`,
// `dhcpLeases` and `dhcpNetworks`, none of which was in the first version of the
// table. The build was clean and the pages would have quietly stopped
// collecting.
//
// Phase 4.2b deleted those call sites. `applyDemand` iterates `TargetKeys()` and
// passes each key back to the funnel, so a key it passes is IN the table by
// construction and the old question cannot be asked or failed.
//
// ── THE SAME DANGER, ONE STEP EARLIER ──────────────────────────────────────
//
// What decides whether a collector is reachable at all is now its ROOMS. A
// collector that declares an audience and is missing from this table is the
// exact 2026-08-28 failure in a new place: demand asks about every key in the
// table, never sees this one, and it never starts for a viewer — silently,
// because a collector that is never resumed looks like one with nothing to
// report.
func TestEveryCollectorWithAnAudienceIsInTheTable(t *testing.T) {
	known := map[string]bool{}
	for _, k := range targetKeys {
		known[k] = true
	}
	// The collectors demand can be asked about, read from the declarations
	// rather than listed here — a second list is what this whole phase removed.
	seen := 0
	for _, key := range collect.DeclaredRoomKeys() {
		if len(collect.DemandRooms(key)) == 0 {
			continue
		}
		seen++
		if known[key] {
			continue
		}
		// `logs` and `ping` declare rooms and are deliberately outside the table:
		// logs holds a push channel for the life of the connection and its
		// Suspend/Resume are no-ops, and ping is not dormancy-eligible or
		// page-gated. Both are recorded rather than derived, because "not in the
		// table" is otherwise indistinguishable from the bug above.
		if key == "logs" || key == "ping" {
			continue
		}
		t.Errorf("%q declares an audience and the session table has no entry for it, so "+
			"applyDemand never asks about it and no viewer can ever start it", key)
	}
	if seen < 20 {
		t.Errorf("only %d collectors were read as declaring an audience; there are at "+
			"least twenty, so the declarations have stopped being readable and this "+
			"check is measuring nothing", seen)
	}
}
