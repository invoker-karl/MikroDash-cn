package verify

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"mikrodash/internal/pages"
)

// TestPageOwnershipIsReal checks `pages.Page.Collector` against two independent
// facts: the collector registry, and the rooms the collectors actually emit to.
//
// ── WHY THIS LIVES HERE AND NOT IN internal/pages ───────────────────────────
//
// `internal/pages` imports NOTHING, and three commands depend on that — it is
// read by `cmd/webbuild`, `cmd/pagesgen` and `internal/server`, and giving it a
// dependency on `internal/collection` would drag the registry into the frontend
// build. So the cross-check goes where cross-checks go.
//
// ── THE THIRD ASSERTION IS THE ONE THAT MAKES THIS WORTH HAVING ─────────────
//
// Checking that an owner names a real registry key proves only that somebody
// typed a valid string. Checking that the named collector EMITS TO `page-<key>`
// proves the declaration describes the running app — and that is the property
// phase 4.2 wants to build on, because the whole point of a view declaring its
// rooms is that nothing else gets to have a second opinion about them.
func TestPageOwnershipIsReal(t *testing.T) {
	root := repoRoot(t)

	// ── 1. EVERY OWNER NAMES A REAL REGISTRY KEY ────────────────────────────
	var tables struct {
		Registry []struct {
			Key string `json:"key"`
		} `json:"registry"`
	}
	raw := mustRead(t, filepath.Join(root, "internal", "collection", "collection_tables.json"))
	if err := json.Unmarshal([]byte(raw), &tables); err != nil {
		t.Fatalf("collection_tables.json: %v", err)
	}
	known := map[string]bool{}
	for _, c := range tables.Registry {
		known[c.Key] = true
	}

	owned := 0
	byCollector := map[string]string{}
	for _, p := range pages.All {
		if p.Collector == "" {
			continue
		}
		owned++
		if !known[p.Collector] {
			t.Errorf("page %q is owned by %q, which is not a collector in the registry",
				p.Key, p.Collector)
		}
		// ── 2. NO TWO PAGES SHARE AN OWNER ──────────────────────────────
		//
		// Ownership is "the collector this page exists to show", and two pages
		// cannot both be that. `ifStatus` is the case it guards: it FEEDS
		// `interfaces` and `network-topology` both, and owns only the first.
		if other, dup := byCollector[p.Collector]; dup {
			t.Errorf("%q owns both %q and %q. Feeding two pages is normal; OWNING two "+
				"is not — ownership is the page a collector exists for.",
				p.Collector, other, p.Key)
		}
		byCollector[p.Collector] = p.Key
	}
	if owned == 0 {
		t.Fatal("no page declares an owner — this check is reading nothing")
	}

	// ── 3. THE OWNER ACTUALLY EMITS TO page-<key> ───────────────────────────
	//
	// Scanned from the emit literals, which is the same source
	// `collectorRooms` reads for the blur-suspend guard. A declaration that has
	// stopped being true fails here rather than becoming folklore.
	//
	// TWO EXCEPTIONS, both real and both recorded rather than special-cased in
	// the data:
	//
	//	logs           builds its room list at runtime, so there is no literal.
	//	dhcpNetworks   emits "page-dhcp,dash-card-network" — the page room is
	//	               there, it simply shares the string with a card.
	//
	// The second is not really an exception; it is why this matches a SUBSTRING
	// of the emit rather than the whole argument.
	exempt := map[string]string{
		"logs": "builds its room list at runtime, so there is no literal to scan",
	}

	dir := filepath.Join(root, "internal", "collect")
	rooms := map[string]string{} // file name -> its source
	for _, name := range collectGoFiles(t, dir) {
		if isTestSource(name) {
			continue
		}
		rooms[name] = mustRead(t, filepath.Join(dir, name))
	}

	// ── THE OWNER'S OWN FILE, NOT ANY FILE ──────────────────────────────────
	//
	// THE FIRST VERSION OF THIS CHECK SEARCHED EVERY COLLECTOR FILE for an emit
	// to `page-<key>`, and it was theatre: re-declaring `interfaces` as owned by
	// `netwatch` PASSED, because `ifstatus.go` still emitted to
	// `page-interfaces` and the scan did not care who did. It asserted "somebody
	// feeds this page", which is true of every page by construction.
	//
	// Caught by mutation, not by review. The fix is to scan ONE file, which needs
	// collector-key -> filename written down; `scheduled_test.go` keeps the same
	// map for the same reason -- the names differ often enough (conns/connections,
	// rosusers/rosusers.go, ifStatus/ifstatus.go) that deriving it would be a
	// second source of truth.
	fileOf := map[string]string{
		"dns": "dns.go", "bridges": "bridges.go", "vlans": "vlans.go",
		"wan": "wan.go", "packages": "packages.go", "routing": "routing.go",
		"dhcpNetworks": "dhcpnetworks.go", "ppp": "ppp.go", "vpn": "vpn.go",
		"rosusers": "rosusers.go", "queues": "queues.go", "firewall": "firewall.go",
		"wifi": "wifi.go", "capsman": "capsman.go", "ifStatus": "ifstatus.go",
		"logs": "logs.go", "topology": "topology.go", "wireless": "wireless.go",
		"bandwidth": "bandwidth.go", "conns": "connections.go",
	}

	emitRoom := regexp.MustCompile(`emit\("([^"]*)"`)
	var unproven []string
	for _, p := range pages.All {
		if p.Collector == "" {
			continue
		}
		file, ok := fileOf[p.Collector]
		if !ok {
			t.Errorf("page %q is owned by %q and that collector has no file here. Add it, "+
				"or the emit check silently skips this page.", p.Key, p.Collector)
			continue
		}
		src, ok := rooms[file]
		if !ok {
			t.Errorf("%s is named as %s's source and does not exist", file, p.Collector)
			continue
		}
		if why, exempted := exempt[p.Collector]; exempted {
			t.Logf("%s: exempt from the emit check — %s", p.Collector, why)
			continue
		}
		want := "page-" + p.Key
		found := false
		for _, m := range emitRoom.FindAllStringSubmatch(src, -1) {
			for _, room := range strings.Split(m[1], ",") {
				if strings.TrimSpace(room) == want {
					found = true
				}
			}
		}
		if !found {
			unproven = append(unproven, p.Key+" (owner "+p.Collector+", "+file+")")
		}
	}
	sort.Strings(unproven)
	if len(unproven) > 0 {
		t.Errorf("these pages declare an owner and nothing emits to their room: %v.\n"+
			"Either the declaration is wrong, or the collector stopped emitting there — "+
			"and the second is silent, because the page simply never updates.", unproven)
	}
	t.Logf("%d of %d pages declare an owner; %d pages own nothing and cannot be emptied "+
		"by a collector", owned, len(pages.All), len(pages.All)-owned)
}
