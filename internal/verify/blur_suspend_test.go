package verify

import (
	"mikrodash/internal/collect"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestBlurSuspendGuards: a collector that emits to more than one room must not be
// suspended by a bare page blur.
//
// ── THE BUG THIS EXISTS FOR ─────────────────────────────────────────────────
//
// `pageBlur` stops a page's collectors when the operator navigates away. That is
// correct only while the page is the collector's ONLY audience. Several
// collectors also feed a dashboard card, and for a viewer who never opens the
// owning page the card is the whole reason the collector runs -- so blurring the
// page starves the card.
//
// The live app never had this: its `_updatePageStream` counted occupancy across
// ALL of a collector's stream rooms. This port has one funnel per page, so the
// property has to be asserted instead of inherited. `suspendIfNoRoomOccupied` is
// the guarded form.
//
// It has caught the same defect four times -- dhcpNetworks, bandwidth, vpn,
// firewall -- and a fifth on 2026-09-01, when `routing` gained `page-dashboard`
// as a second room so the Routes and BGP cards could be fed.
func TestBlurSuspendGuards(t *testing.T) {
	root := repoRoot(t)

	wsSrc := mustRead(t, filepath.Join(root, "internal", "server", "ws.go"))
	blur := sliceBetween(t, wsSrc,
		"func (cn *conn) pageBlur(",
		"func (cn *conn) trafficSelectDefault(")
	// The anchors are function signatures, so drift is silent: the slice would
	// still be a string, just the wrong one. Counting the cases proves it is
	// still pageBlur's body.
	if n := strings.Count(blur, "\n\tcase \""); n < 12 {
		t.Fatalf("the pageBlur slice holds %d cases, expected at least 12 — the anchors drifted "+
			"and this test is reading the wrong function", n)
	}

	roomsByFile := collectorRooms(t, filepath.Join(root, "internal", "collect"))
	fileOfType := suspendReceivers(t, filepath.Join(root, "internal", "collect"), roomsByFile)
	typeOfAccessor := sessionAccessors(t, mustRead(t, filepath.Join(root, "internal", "session", "session.go")))

	direct := regexp.MustCompile(`cn\.rsession\.(\w+)\(\)\.Suspend\(\)`)
	checked := 0
	for _, m := range direct.FindAllStringSubmatch(blur, -1) {
		acc := m[1]
		typ, ok := typeOfAccessor[acc]
		if !ok {
			continue
		}
		file, ok := fileOfType[typ]
		if !ok {
			continue
		}
		rooms := roomsByFile[file]
		if len(rooms) == 0 {
			continue
		}
		checked++
		if len(rooms) > 1 {
			t.Errorf("%s() is suspended directly in pageBlur, but its collector emits to %d rooms "+
				"(%s). A page blur says nothing about whether anybody is still watching the "+
				"others — use suspendIfNoRoomOccupied.", acc, len(rooms), strings.Join(sorted(rooms), ", "))
		}
	}

	// ── THE TEST MUST PROVE ITS OWN DATA IS REAL ────────────────────────────
	//
	// Every direct suspend being single-room is the PASSING state, so the
	// failure branch never fires on a clean run. A mutation that broke the
	// room-reading would therefore survive, because the test would still pass --
	// by looking at nothing. Asserting that multi-room collectors were actually
	// FOUND is what makes a clean run mean something.
	multi := 0
	for _, rooms := range roomsByFile {
		if len(rooms) > 1 {
			multi++
		}
	}
	if multi < 3 {
		t.Fatalf("only %d collectors were read as emitting to more than one room; there are at "+
			"least four, so the emit-reading has stopped matching and this test checks nothing", multi)
	}
	for _, f := range []string{"connections.go", "dhcpnetworks.go", "bandwidth.go", "vpn.go"} {
		if len(roomsByFile[f]) < 2 {
			t.Fatalf("%s was not read as multi-room; it is one of the four this test was written for", f)
		}
	}
	if checked == 0 {
		t.Fatal("no direct suspend was resolved to a collector — the accessor chain broke")
	}
	t.Logf("%d direct suspends, all single-room; %d multi-room collectors found", checked, multi)
}

var (
	emitLiteral  = regexp.MustCompile(`\.emit\(\s*"([^"]*)"\s*,\s*"([^"]+)"`)
	emitConst    = regexp.MustCompile(`\.emit\((\w+),\s*"([^"]+)"`)
	suspendRecv  = regexp.MustCompile(`func \(\w+ \*(\w+)\) Suspend\(\)`)
	sessAccessor = regexp.MustCompile(`func \(s \*Session\) (\w+)\(\) \*collect\.(\w+)`)
)

// collectorRooms maps a collector file to the set of rooms it emits to.
//
// ── READS THE DECLARATIONS SINCE 4.2, NOT THE SOURCE ────────────────────────
//
// This used to scan `emit("…")` literals with a regex, plus a second regex for
// the two collectors that emitted to a named constant, and it resolved those by
// hunting for the constant's own declaration. Every audience is declared in
// `internal/collect/rooms.go` now, so the values can simply be asked for.
//
// Three things got better and none got worse. There is no pattern to drift when
// a call site is reformatted. `logs` and `talkers` are covered like everything
// else rather than by best-effort constant resolution. And the rooms are real
// values, so a typo inside one is a compile error in `collect` rather than a
// silently unmatched regex here.
//
// STILL KEYED BY FILE, because both callers reach a collector through its Go
// type and `suspendReceivers` resolves a type to a file. The key-to-file map is
// the only hand-written part, and it is checked below.
func collectorRooms(t *testing.T, dir string) map[string]map[string]bool {
	t.Helper()
	// collector key -> the file its type is declared in. Explicit for the reason
	// `scheduled_test.go` gives: conns/connections.go and rosusers differ often
	// enough that deriving it would be a second source of truth.
	fileOf := map[string]string{
		"bandwidth": "bandwidth.go", "bridges": "bridges.go", "capsman": "capsman.go",
		"conns": "connections.go", "dhcpNetworks": "dhcpnetworks.go", "dns": "dns.go",
		"firewall": "firewall.go", "ifStatus": "ifstatus.go", "logs": "logs.go",
		"netwatch": "netwatch.go", "packages": "packages.go", "ping": "ping.go",
		"ppp": "ppp.go", "queues": "queues.go", "rosusers": "rosusers.go",
		"routing": "routing.go", "talkers": "talkers.go", "topology": "topology.go",
		"vlans": "vlans.go", "vpn": "vpn.go", "wan": "wan.go", "wifi": "wifi.go",
		"wireless": "wireless.go",
	}
	out := map[string]map[string]bool{}
	for key, file := range fileOf {
		rooms := collect.RoomsOf(key)
		if len(rooms) == 0 {
			t.Errorf("%s is named here and declares no rooms — the map and rooms.go "+
				"have drifted, and this helper would silently report it as unguarded", key)
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			t.Errorf("%s is named as %s's source and does not exist", file, key)
			continue
		}
		out[file] = map[string]bool{}
		for _, r := range rooms {
			out[file][r] = true
		}
	}
	// The router-wide emits are not declared as rooms and never were guardable;
	// the callers only ever asked "does this collector feed more than one room",
	// for which the router-wide entry was noise.
	if len(out) == 0 {
		t.Fatal("no collector rooms were read — the declarations are not reachable")
	}
	return out
}

func suspendReceivers(t *testing.T, dir string, known map[string]map[string]bool) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name := range known {
		src := mustRead(t, filepath.Join(dir, name))
		for _, m := range suspendRecv.FindAllStringSubmatch(src, -1) {
			out[m[1]] = name
		}
	}
	return out
}

func sessionAccessors(t *testing.T, src string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range sessAccessor.FindAllStringSubmatch(src, -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatal("no Session collector accessors were read — the scan is broken")
	}
	return out
}

func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			out = append(out, n)
		}
	}
	return out
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// sliceBetween returns the text from `from` up to `to`, failing loudly when
// either anchor is gone — an anchor that silently misses would leave every
// assertion below inspecting the wrong code.
func sliceBetween(t *testing.T, src, from, to string) string {
	t.Helper()
	i := strings.Index(src, from)
	if i < 0 {
		t.Fatalf("anchor lost: %q", from)
	}
	j := strings.Index(src[i:], to)
	if j < 0 {
		t.Fatalf("anchor lost: %q, which should follow %q", to, from)
	}
	return src[i : i+j]
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// TestGuardedSuspendCoversEveryDashboardRoom is the half of the blur-suspend
// audit that was missing, and it was missing in the shape that let a real bug
// through for the whole life of this port.
//
// ── WHAT TestBlurSuspendGuards CANNOT SEE ───────────────────────────────────
//
// That test matches `cn.rsession.X().Suspend()` inside pageBlur and objects when
// a multi-room collector is suspended DIRECTLY. It says nothing at all about the
// guarded form: once a call is written as `suspendIfNoRoomOccupied(...)` it is
// accepted without anybody checking WHICH rooms were passed. So the audit
// written for this defect class approved a guard that omitted the very room the
// guard exists to protect.
//
// `suspendConnsIfIdle` passed `page-connections` and `page-bandwidth` and not
// `dash-card-connections`, while `connections.go` emits to
// `page-connections,dash-card-connections`. Return to the dashboard from either
// of those pages and, one idle grace later, the collector was suspended with a
// viewer still watching the card — which is exactly what the operator saw.
//
// ── THE PROPERTY ────────────────────────────────────────────────────────────
//
// Every `dash-card-*` room a collector emits to must appear in the room list of
// every guarded suspend of that collector. That is the rule `suspendIfNoRoomOccupied`'s
// own header states — "a `dash-card-*` room emptying is not [what triggers a
// blur], so it must be TESTED rather than assumed" — asserted rather than
// described.
//
// SINCE 4.2 THE ORIGINAL PROPERTY IS STRUCTURAL and this checks the new risk
// instead — see the note above the match below.
//
// The whole of ws.go is scanned rather than just pageBlur's body, because the
// conns guard lives in a helper — which is precisely how it escaped the first
// audit.
func TestGuardedSuspendCoversEveryDashboardRoom(t *testing.T) {
	root := repoRoot(t)
	wsSrc := mustRead(t, filepath.Join(root, "internal", "server", "ws.go"))

	// Both spellings of the receiver: `cn.rsession` in a pageBlur case, `rs` in a
	// helper on Server. A third spelling would go unchecked, so the count guard
	// at the bottom is what keeps that from being silent.
	resolved := 0
	// ── WHAT THIS ASSERTS SINCE 4.2, AND WHY IT CHANGED ─────────────────────
	//
	// It used to check that a guarded suspend LISTED every dashboard-card room
	// its collector emits to — because the list was hand-written and could omit
	// one, which is how the Connections card came to starve.
	//
	// That property is now STRUCTURAL. `collect.Others(key, page)` returns the
	// whole declared audience minus the blurred page, and a card room is never a
	// page room, so a card can no longer be dropped. The old assertion would pass
	// for free, which makes it worse than useless: a green check nobody can fail.
	//
	// So it is re-aimed at the failure the new shape actually has. The guard names
	// the collector by KEY and suspends it by ACCESSOR, and nothing connects the
	// two — `collect.Others("vpn", …)` beside `Wireless().Suspend` compiles, reads
	// perfectly, and guards the wrong collector. That is one copy-paste away, and
	// it is silent: the wrong collector's rooms are consulted, so the right one
	// suspends while somebody is watching it.
	guarded := regexp.MustCompile(
		`collect\.Others\("(\w+)", "([a-z-]*)"\), (?:cn\.rsession|rs)\.(\w+)\(\)\.Suspend`)

	// accessor -> the collector key it must be guarded by.
	keyOfAccessor := map[string]string{
		"Routing": "routing", "DHCPNetworks": "dhcpNetworks", "VPN": "vpn",
		"Firewall": "firewall", "Wireless": "wireless", "Bandwidth": "bandwidth",
		"Conns": "conns",
	}

	// FLATTENED, and comments dropped first. The conns guard puts its argument
	// and its receiver on separate lines with a comment between them, so a match
	// against the raw source finds seven of eight -- and a silently-skipped guard
	// is exactly what this test exists to prevent.
	flat := strings.Join(strings.Fields(stripGoComments(wsSrc)), " ")
	for _, m := range guarded.FindAllStringSubmatch(flat, -1) {
		key, page, acc := m[1], m[2], m[3]
		resolved++

		want, ok := keyOfAccessor[acc]
		if !ok {
			t.Errorf("a guarded suspend of %s() is not in keyOfAccessor, so the key it "+
				"passes goes unchecked. Add it.", acc)
			continue
		}
		if key != want {
			t.Errorf("the guarded suspend of %s() asks about collector %q, but %s is %q.\n"+
				"The guard would consult the WRONG collector's rooms and suspend this one "+
				"while somebody is still watching it — silently, because both keys are real.",
				acc, key, acc, want)
		}
		if len(collect.RoomsOf(key)) == 0 {
			t.Errorf("%s() is guarded on %q, which declares no rooms — so the guard waits "+
				"on nothing and always suspends", acc, key)
		}
		// The page named must be one this collector actually feeds, or the
		// subtraction does nothing and the guard is stricter than intended.
		if page != "" {
			feeds := false
			for _, r := range collect.RoomsOf(key) {
				if r == "page-"+page {
					feeds = true
				}
			}
			if !feeds {
				t.Errorf("%s() is guarded against a blur of page %q, which %q does not "+
					"feed. Nothing is subtracted, so the collector never suspends.", acc, page, key)
			}
		}
	}

	// ── THE TEST MUST PROVE ITS OWN DATA IS REAL ────────────────────────────
	//
	// Every guard being correct is the PASSING state, so on a clean run the
	// failure branches never fire and a broken match would look identical to a
	// clean repository.
	if resolved < 8 {
		t.Fatalf("only %d guarded suspends matched; ws.go has eight, so the call shape "+
			"has changed and this test checks nothing", resolved)
	}
}

// setOf is the inverse of the room maps this file reads: a slice back to a set,
// so the failure message can reuse `sorted`.
func setOf(v []string) map[string]bool {
	out := make(map[string]bool, len(v))
	for _, s := range v {
		out[s] = true
	}
	return out
}
