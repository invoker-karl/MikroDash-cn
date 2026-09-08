package verify

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"mikrodash/internal/pages"
)

// TestBlurGuardRoomsMatchTheEmits: the room list `pageBlur` guards on must be
// exactly the collector's OTHER rooms — the ones it emits to besides the page
// being blurred.
//
// ── THE TWO FACTS THIS PUTS BACK TOGETHER ───────────────────────────────────
//
// A collector's rooms are written in `internal/collect`, at its emit. The rooms
// `pageBlur` waits on are written again, by hand, in `internal/server/ws.go`.
// Two statements of one fact, and this repository has watched them disagree
// FIVE times — dhcpNetworks, bandwidth, vpn, firewall, and routing on
// 2026-08-31 — each time as a card that silently stopped updating for anybody
// who had visited the owning page and left.
//
// `TestBlurSuspendGuards` already catches the coarse form: a multi-room
// collector suspended by a BARE blur with no guard at all. It cannot catch the
// finer one, which is a guard that exists and names the wrong rooms — and a
// guard naming too few rooms fails exactly the same way as no guard.
//
// ── WHY A GATE RATHER THAN DELETING THE SECOND FACT ─────────────────────────
//
// Phase 4.2 wants the guard to READ the collector's declaration, so the second
// list stops existing. That is a change to the delivery path in seven places,
// and it should be made against evidence that the two agree today rather than
// on the assumption. This is that evidence, and it keeps its value afterwards
// as the check that the derivation stayed faithful.
func TestBlurGuardRoomsMatchTheEmits(t *testing.T) {
	root := repoRoot(t)
	wsSrc := mustRead(t, filepath.Join(root, "internal", "server", "ws.go"))
	blur := sliceBetween(t, wsSrc,
		"func (cn *conn) pageBlur(",
		"func (cn *conn) trafficSelectDefault(")

	roomsByFile := collectorRooms(t, filepath.Join(root, "internal", "collect"))

	// collector key -> the file its rooms live in. Explicit for the reason
	// `scheduled_test.go` gives: conns/connections.go and rosusers/rosusers.go
	// differ often enough that deriving it would be a second source of truth.
	fileOf := map[string]string{
		"routing": "routing.go", "dhcpNetworks": "dhcpnetworks.go", "vpn": "vpn.go",
		"firewall": "firewall.go", "wireless": "wireless.go", "bandwidth": "bandwidth.go",
	}
	// The accessor `pageBlur` reaches each one by, so a case can be matched to a
	// collector without parsing Go.
	accessorOf := map[string]string{
		"Routing": "routing", "DHCPNetworks": "dhcpNetworks", "VPN": "vpn",
		"Firewall": "firewall", "Wireless": "wireless", "Bandwidth": "bandwidth",
	}

	// Each guarded case, as `case "<page>":` ... `[]string{...}, cn.rsession.X()`.
	caseRe := regexp.MustCompile(`case "([a-z-]+)":`)
	guardRe := regexp.MustCompile(`\[\]string\{([^}]*)\}, cn\.rsession\.(\w+)\(\)`)

	// Walk the blur body, remembering which case we are inside.
	type guard struct{ page, accessor, list string }
	var guards []guard
	pos := 0
	current := ""
	for pos < len(blur) {
		nextCase := caseRe.FindStringSubmatchIndex(blur[pos:])
		nextGuard := guardRe.FindStringSubmatchIndex(blur[pos:])
		if nextGuard == nil {
			break
		}
		if nextCase != nil && nextCase[0] < nextGuard[0] {
			current = blur[pos+nextCase[2] : pos+nextCase[3]]
			pos += nextCase[1]
			continue
		}
		guards = append(guards, guard{
			page:     current,
			list:     blur[pos+nextGuard[2] : pos+nextGuard[3]],
			accessor: blur[pos+nextGuard[4] : pos+nextGuard[5]],
		})
		pos += nextGuard[1]
	}

	if len(guards) < 6 {
		t.Fatalf("found %d guarded blur cases, expected at least 6 — the pattern has "+
			"drifted and this check is reading almost nothing", len(guards))
	}

	for _, g := range guards {
		key, ok := accessorOf[g.accessor]
		if !ok {
			t.Errorf("case %q guards %s(), which is not in accessorOf. A new guarded "+
				"collector must be added here or its rooms go unchecked.", g.page, g.accessor)
			continue
		}
		declared := roomsByFile[fileOf[key]]
		if len(declared) == 0 {
			t.Errorf("%s emits to no rooms this check can see (%s)", key, fileOf[key])
			continue
		}

		// THE EXPECTATION: everything it emits to, except the page being blurred
		// and except the router-wide empty room.
		//
		// The empty room cannot be guarded on and its absence is a JUDGEMENT, not
		// an oversight — every viewer of the router occupies it, so testing it
		// would mean never suspending at all. `dhcpNetworks` is the case, and
		// ws.go carries the reasoning at the call site.
		var want []string
		for _, r := range sorted(declared) {
			// `collectorRooms` renders the empty room as `<router-wide>`, which
			// is what an emit with no room means: every viewer of this router.
			if r == "" || r == "<router-wide>" || r == "page-"+g.page {
				continue
			}
			want = append(want, r)
		}
		var got []string
		for _, part := range strings.Split(g.list, ",") {
			if p := strings.Trim(strings.TrimSpace(part), `"`); p != "" {
				got = append(got, p)
			}
		}
		sort.Strings(want)
		sort.Strings(got)
		if strings.Join(want, ",") != strings.Join(got, ",") {
			t.Errorf("blurring %q guards %s on %v, but it emits to %v — so the guard "+
				"waits on %v.\nA guard that names too FEW rooms starves a card exactly "+
				"as a missing guard does; one that names too many never suspends.",
				g.page, key, got, sorted(declared), want)
		}
	}

	// The pages named must be real, or a renamed key silently disables a guard.
	real := map[string]bool{}
	for _, p := range pages.All {
		real[p.Key] = true
	}
	for _, g := range guards {
		if !real[g.page] {
			t.Errorf("pageBlur guards a case %q that is not a page key any more", g.page)
		}
	}
	t.Logf("%d guarded blur cases, all agreeing with their collector's emits", len(guards))
}
