package session

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"testing"
)

// TestPollTargetsCoversEveryRetunableCollector.
//
// ── THE HALF THE OTHER LEDGER CANNOT SEE ───────────────────────────────────
//
// `internal/collect/retime_test.go` asserts that every collector the live route
// re-tunes has a `SetPollMs` on some Go type. That is necessary and it is not
// enough: the method can exist and the SESSION can fail to offer the collector,
// in which case `applyRetunes` finds no target and does nothing.
//
// The symptom is precisely the one that ledger names — "the setting would save,
// the payload would report the new period, and the collector would keep polling
// at the old one" — with nothing logged anywhere, because a missing key in a map
// is not an error.
//
// FOUND BY MUTATION, 2026-09-10: deleting `arp` from `pollTargets` left the whole
// package green. Every other key was equally unguarded; `arp` was simply the one
// being added when the gap was noticed.
//
// SOURCE-READ, because `pollTargets` reads a Session's collector fields and a
// Session cannot be built without a router. The same trade every lifecycle check
// in this package makes.
func TestPollTargetsCoversEveryRetunableCollector(t *testing.T) {
	b, err := os.ReadFile("../../testdata/settings-apply-cases.json")
	if err != nil {
		t.Fatalf("read the lifted poll map: %v", err)
	}
	var doc struct {
		PollMap map[string]string `json:"pollMap"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.PollMap) == 0 {
		t.Fatal("the lifted poll map is empty, so this test would pass against nothing")
	}

	src, err := os.ReadFile("retune.go")
	if err != nil {
		t.Fatal(err)
	}
	offered := map[string]bool{}
	for _, m := range regexp.MustCompile(`add\("(\w+)"`).FindAllStringSubmatch(string(src), -1) {
		offered[m[1]] = true
	}
	// A BELIEVABILITY FLOOR: if the pattern stops matching, every assertion
	// below passes over an empty set, which is the failure this whole file is
	// about.
	if len(offered) < 20 {
		t.Fatalf("only %d collectors were read out of pollTargets; there are more than "+
			"twenty, so the scan has stopped matching and this check is measuring nothing",
			len(offered))
	}

	var missing []string
	for _, name := range doc.PollMap {
		if !offered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the live route re-tunes %v and pollTargets does not offer them: the "+
			"setting saves, the payload reports the new period, and the collector keeps "+
			"polling at the old one — silently, because a missing map key is not an error.",
			missing)
	}

	// AND THE OTHER DIRECTION. A collector offered here that nothing re-tunes is
	// a target the settings route can never reach, which means either the poll
	// map or this list is wrong.
	live := map[string]bool{}
	for _, name := range doc.PollMap {
		live[name] = true
	}
	var orphan []string
	for name := range offered {
		if !live[name] {
			orphan = append(orphan, name)
		}
	}
	sort.Strings(orphan)
	if len(orphan) > 0 {
		t.Errorf("pollTargets offers %v and no poll setting names them", orphan)
	}
}
