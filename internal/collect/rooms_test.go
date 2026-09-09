package collect

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestNoEmitPassesARoomLiteral is what replaces the bridge.
//
// ── THE INVARIANT THE WHOLE OF 4.2 RESTS ON ─────────────────────────────────
//
// Every audience is declared in rooms.go and read from there. The value of that
// is entirely in COMPLETENESS: one call site left with a literal is one place the
// old two-facts bug can grow back, and it would look perfectly ordinary.
//
// So this is one rule replacing three pattern-matchers. It also removes the
// exemptions the old source-scanning gates carried for `logs` and `talkers`,
// which emitted to named constants and could not be checked at all.
//
// THE ROUTER-WIDE EMIT IS THE ONE ALLOWED LITERAL, and it is not a room: `""`
// means every viewer of this router. Five collectors use it.
func TestNoEmitPassesARoomLiteral(t *testing.T) {
	files, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// Any emit whose first argument is a non-empty string literal.
	bad := regexp.MustCompile(`emit\("[^"]+"`)
	found := 0
	for _, f := range files {
		n := f.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		src, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		found++
		for _, m := range bad.FindAllString(stripComments(string(src)), -1) {
			t.Errorf("%s: %s — rooms belong in rooms.go. A literal here is a second "+
				"statement of the audience, and the blur guard reads the first one.", n, m)
		}
	}
	if found < 20 {
		t.Fatalf("read %d collector files; this check has stopped seeing the package", found)
	}
}

// stripComments removes // lines so a comment QUOTING an old emit does not fail
// the check that forbids it.
//
// The same trap has now been hit twice in this repository — the credential
// scanner reading a comment about proplists, and a dormancy gate reading its own
// explanation — so it is handled here rather than discovered a third time.
func stripComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "//") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestEveryDeclaredRoomIsAPageOrACard: a room name is a protocol term shared
// with the browser, and a typo in one is silent — the emit goes to a room nobody
// has joined, and the page simply never updates.
func TestEveryDeclaredRoomIsAPageOrACard(t *testing.T) {
	for _, key := range declaredKeys() {
		for _, r := range RoomsOf(key) {
			if !strings.HasPrefix(r, "page-") && !strings.HasPrefix(r, "dash-card-") {
				t.Errorf("%s declares room %q, which is neither a page room nor a card "+
					"room. Those are the only two namespaces the browser joins.", key, r)
			}
		}
	}
}

// TestOthersDropsTheBlurredPageAndNothingElse pins the calculation the seven
// guard call sites used to spell out by hand. Getting it wrong in either
// direction is a real defect: too few rooms starves a card, too many means the
// collector never suspends and keeps asking a router nobody is watching.
func TestOthersDropsTheBlurredPageAndNothingElse(t *testing.T) {
	cases := []struct {
		key, page string
		want      []string
	}{
		{"vpn", "vpn", []string{"dash-card-vpn"}},
		{"routing", "routing", []string{"page-dashboard"}},
		{"routing", "dashboard", []string{"page-routing"}},
		{"dhcpNetworks", "dhcp", []string{"dash-card-network"}},
		{"wireless", "wifi-clients", []string{"dash-card-wireless"}},
		// No page blurred: the whole audience, AND NOTHING ELSE.
		//
		// `page-bandwidth` was here until 2026-09-09, as the one `keepAliveFor`
		// entry: `bandwidth` read the connection table `conns` deposited in
		// `ConnTable`, so suspending `conns` starved a page it never emits to.
		// Both collectors subscribe to the menu now and `bandwidth` holds its own
		// demand, so a suspended `conns` starves nothing.
		//
		// THIS IS A BEHAVIOUR CHANGE AND IT IS THE POINT: `conns` may now suspend
		// on a Connections blur while somebody is on Bandwidth, which is a
		// collector that stops asking a router about a page nobody is looking at.
		{"conns", "", []string{"dash-card-connections", "page-connections"}},
		// A page that this collector does not feed changes nothing.
		{"vpn", "dns", []string{"dash-card-vpn", "page-vpn"}},
	}
	for _, c := range cases {
		got := Others(c.key, c.page)
		sort.Strings(got)
		want := append([]string(nil), c.want...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("Others(%q, %q) = %v, want %v", c.key, c.page, got, want)
		}
	}
}

func declaredKeys() []string {
	return []string{
		"bandwidth", "bridges", "capsman", "conns", "dhcpNetworks", "dns",
		"firewall", "ifStatus", "logs", "netwatch", "packages", "ping", "ppp",
		"queues", "rosusers", "routing", "talkers", "topology", "vlans", "vpn",
		"wan", "wifi", "wireless",
	}
}

// TestDeclaredKeysCoverRoomsOf stops the list above drifting from the switch it
// describes — a key dropped from one and not the other makes the two checks
// above quietly stop looking at it.
func TestDeclaredKeysCoverRoomsOf(t *testing.T) {
	for _, key := range declaredKeys() {
		if len(RoomsOf(key)) == 0 {
			t.Errorf("declaredKeys names %q and RoomsOf returns nothing for it", key)
		}
	}
	if got := len(declaredKeys()); got != 23 {
		t.Errorf("declaredKeys has %d entries, expected 23 — a collector gained or lost "+
			"an audience and one of these lists was not updated", got)
	}
}
