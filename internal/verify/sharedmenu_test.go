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
		// ROUTED SINCE 3.2d, and this entry carried the OPPOSITE claim until
		// 2026-09-09. `connections.go` and `bandwidth.go` both subscribe, with
		// byte-identical proplists, so it is one read with two deliveries. The
		// reason it used to hold -- "connTable passes the PARSED snapshot" --
		// was wrong in its premise as well as out of date: `ConnTable.Latest`
		// handed back RAW rows. See L.3 for why nothing caught it.
		"/ip/firewall/connection/print": "",

		"/interface/monitor-traffic": "one side holds a stream and the other takes a bounded " +
			"=once= measurement, so a by-menu entry would hand one the other's answer. " +
			"SLATED FOR B.1: a stream can BACK an entry when its rows are successive " +
			"readings of a keyed value, which these are -- at which point this becomes " +
			"routed and the entry moves up",
		"/tool/ping": "ONE stream and one BOUNDED per-device read (topology sends =count=1), " +
			"not two streams. Parameterised by address, so the menu is not the question -- " +
			"and permanently unroutable for a second reason: each row is a DISTINCT " +
			"measurement the collector counts into min/max/avg/loss, not a fresh reading " +
			"of one value. A rolling entry would report 0% loss for ever and fail nothing",
	}

	consumers := sharedMenus(t, dir)
	subscribed := subscribedMenus(t, dir)

	for menu, files := range consumers {
		why, listed := ledger[menu]
		if !listed {
			t.Errorf("%s is read by %v and is not in the shared-menu ledger. Route it through "+
				"the cache (see internal/collect/cache.go) or record here why it cannot be.",
				menu, files)
			continue
		}
		if why != "" {
			// ── L.3: AN EXEMPTION IS RE-CHECKED, NOT TAKEN ON TRUST ────────
			//
			// This used to `continue` here, and that one line is how
			// `/ip/firewall/connection/print` spent a phase claiming it could
			// not be routed while both its consumers were subscribed to it. An
			// entry with a reason was never compared against reality again, so a
			// closed gap could sit here indefinitely saying the opposite -- the
			// exact failure "a recorded gap that has CLOSED is also a failure"
			// exists to prevent, missing from the one branch nobody applied it
			// to.
			//
			// It cost more than tidiness: a session on 2026-09-09 read this
			// entry, believed it, and argued a double-parse objection that had
			// been measured wrong months earlier.
			if f, ok := subscribed[menu]; ok {
				t.Errorf("%s is recorded as unroutable, but %s SUBSCRIBES to it. The "+
					"exemption has closed; move it to the routed list.\n(reason held: %q)",
					menu, f, why)
			}
			continue
		}
		if _, ok := subscribed[menu]; ok {
			continue // routed by subscription
		}
		for _, f := range files {
			if !strings.Contains(mustRead(t, filepath.Join(dir, f)), "readVia(") {
				t.Errorf("%s is marked routed, but %s neither calls readVia nor subscribes "+
					"to it. Either it stopped reading through the cache or the entry is "+
					"wrong.", menu, f)
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

// subscribedMenus maps each menu a collector routes by SUBSCRIPTION to the file
// that does it.
//
// ── L.4: "ROUTED" MEANT `readVia` AND THAT PREDATES THE SCHEDULER ───────────
//
// Phase 1 routed a menu by calling `readVia` on the cache, so looking for that
// call was the whole test. Phase 3 gave collectors a second way in: a
// `scheduled` literal naming a menu, which subscribes and is delivered to. That
// is routing by any measure -- one read, many deliveries -- and neither
// `connections.go` nor `bandwidth.go` contains `readVia(` at all.
//
// So marking `/ip/firewall/connection/print` routed would have failed this test
// for entirely the wrong reason, and the fix for L.2 needed this one first.
//
// ── AND IT ANSWERS A QUESTION `readVia` CANNOT ─────────────────────────────
//
// This file's header records a limit: routing usually lives in a collector's
// `read` helper, so the scan "cannot tell WHICH menu a given readVia serves".
// A subscription does not have that problem -- `scheduled{menu: xxxCmd.Path}`
// names its menu, and `xxxCmd` is declared with a literal path in the same file.
// Resolving one to the other gives an exact menu-to-file mapping, which is what
// makes the L.3 inverse check possible at all: "this menu is not routed" is only
// assertable when routing can be located precisely.
//
// The limit therefore still stands for the `readVia` half and no longer stands
// for the subscription half, which is now most of them.
func subscribedMenus(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	// `xxxCmd = routeros.Cmd{Path: "/menu"` — the declaration, per file.
	decl := regexp.MustCompile(`(\w+)\s*=\s*routeros\.Cmd\{\s*Path:\s*"(/[^"]+)"`)
	// `menu: xxxCmd.Path` inside a scheduled literal.
	sub := regexp.MustCompile(`menu:\s*(\w+)\.Path`)

	out := map[string]string{}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || isTestSource(filepath.Join(dir, n)) {
			continue
		}
		src := mustRead(t, filepath.Join(dir, n))
		paths := map[string]string{}
		for _, m := range decl.FindAllStringSubmatch(src, -1) {
			paths[m[1]] = m[2]
		}
		for _, m := range sub.FindAllStringSubmatch(src, -1) {
			if p, ok := paths[m[1]]; ok {
				out[p] = n
			}
		}
	}
	// A BELIEVABILITY FLOOR, like sharedMenus'. Phase 3 subscribed 22 of 25
	// collectors, so finding a handful means the pattern stopped matching and
	// every check built on this map silently passes.
	if len(out) < 10 {
		t.Fatalf("found only %d subscribed menu(s); the `menu: xxxCmd.Path` pattern "+
			"has stopped matching and both checks built on it are measuring nothing", len(out))
	}
	return out
}
