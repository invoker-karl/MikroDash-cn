package verify

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEverySharedMenuIsRoutedOrExplained is the ledger for step 1.4 of the
// collector rewrite: every RouterOS menu read by more than one collector either
// goes through the coalescing cache, or says here why it cannot.
//
// ── WHY IT NEEDS A LEDGER ───────────────────────────────────────────────────
//
// Sharing is invisible at both ends. A collector that starts reading a menu
// another one already reads costs a router an extra API channel, which is the
// documented bottleneck, and NOTHING anywhere fails. The two files need not even
// mention each other. This is the only place the relation is written down.
//
// ── WHAT IT CAN AND CANNOT PROVE ────────────────────────────────────────────
//
// Honest about its own limits, because a check that overstates them is worse
// than none. The routing usually lives in a collector's `read` helper rather
// than at the call site -- that is where the availability latches are -- so this
// cannot tell WHICH menu a given readVia serves. What it does assert:
//
//	a menu read by two collectors is listed here, or the test fails
//	a listed menu that is no longer shared fails too, so an entry cannot
//	  outlive the situation it describes
//	a menu marked routed whose consumers do not call readVia at all fails
//	a menu marked unroutable with no reason fails
//
// The middle two are what caught nothing yet and are meant to. The first is what
// catches the next collector to reach for a menu somebody already reads.
func TestEverySharedMenuIsRoutedOrExplained(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "internal", "collect")

	// why is empty for a routed menu, and states the obstacle otherwise.
	ledger := map[string]string{
		"/interface/wifi/print":                        "",
		"/interface/wifi/registration-table/print":     "",
		"/interface/wireless/print":                    "",
		"/interface/wireless/registration-table/print": "",
		"/interface/wifi/capsman/remote-cap/print":     "",
		"/interface/vlan/print":                        "",
		"/ip/address/print":                            "",
		"/interface/bridge/port/print":                 "",
		"/interface/bridge/host/print":                 "",
		"/interface/detect-internet/state/print":       "",
		"/system/routerboard/print":                    "",
		"/system/package/update/print":                 "",
		"/ppp/active/print":                            "",
		"/ip/route/print":                              "",
		"/interface/print":                             "",

		"/interface/monitor-traffic": "one side holds a stream, the other takes a single " +
			"measurement; a by-menu cache would hand one the other's answer",
		"/tool/ping": "two streams to different addresses; the menu is not the question being asked",
		"/ip/firewall/connection/print": "already shared, and better: connTable passes the PARSED " +
			"snapshot between connections and bandwidth rather than the read",
	}

	consumers := sharedMenus(t, dir)

	for menu, files := range consumers {
		why, listed := ledger[menu]
		if !listed {
			t.Errorf("%s is read by %v and is not in the shared-menu ledger. Route it through "+
				"the cache (see internal/collect/cache.go) or record here why it cannot be.",
				menu, files)
			continue
		}
		if why != "" {
			continue // recorded as unroutable
		}
		for _, f := range files {
			if !strings.Contains(mustRead(t, filepath.Join(dir, f)), "readVia(") {
				t.Errorf("%s is marked routed, but %s never calls readVia. Either it stopped "+
					"reading through the cache or the entry is wrong.", menu, f)
			}
		}
	}

	for menu, why := range ledger {
		if len(consumers[menu]) > 1 {
			continue
		}
		t.Errorf("the ledger carries %s, which no longer has more than one consumer (%v). "+
			"A recorded gap that has closed is a failure here; drop the entry. (reason held: %q)",
			menu, consumers[menu], why)
	}
}

// sharedMenus maps each RouterOS menu declared in more than one collector to the
// files that declare it.
//
// NON-TEST SOURCE ONLY. A scan that read this directory's tests would find the
// menus quoted in fixtures and stubs and call them consumers, and it would find
// this file's own ledger if it ever moved here.
func sharedMenus(t *testing.T, dir string) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	decl := regexp.MustCompile(`routeros\.Cmd\{\s*Path:\s*"(/[^"]+)"`)
	byMenu := map[string]map[string]bool{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || isTestSource(filepath.Join(dir, n)) {
			continue
		}
		src := mustRead(t, filepath.Join(dir, n))
		for _, m := range decl.FindAllStringSubmatch(src, -1) {
			if byMenu[m[1]] == nil {
				byMenu[m[1]] = map[string]bool{}
			}
			byMenu[m[1]][n] = true
		}
	}
	out := map[string][]string{}
	for menu, files := range byMenu {
		if len(files) < 2 {
			continue
		}
		list := make([]string, 0, len(files))
		for f := range files {
			list = append(list, f)
		}
		sort.Strings(list)
		out[menu] = list
	}
	if len(out) == 0 {
		t.Fatal("no shared menus found at all — the declaration pattern stopped matching")
	}
	return out
}
