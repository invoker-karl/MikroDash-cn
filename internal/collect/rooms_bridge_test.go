package collect

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestDeclaredRoomsMatchTheEmitsStillInTheCode is the BRIDGE for phase 4.2, and
// it is meant to be deleted.
//
// ── WHY A TEMPORARY TEST IS WORTH WRITING ───────────────────────────────────
//
// `rooms.go` restates, as data, the room lists that are still string literals at
// 38 emit call sites. The next commit switches those call sites over to the
// declarations. If a declaration is WRONG, that switch silently changes who
// receives a payload — a page or a card stops updating, and nothing errors.
//
// So the declarations are proved against the literals BEFORE anything depends on
// them. That is the whole job of this file: it exists for exactly one commit,
// and `TestThisBridgeIsStillNeeded` below fails once the literals are gone, which
// is the signal to delete it.
func TestDeclaredRoomsMatchTheEmitsStillInTheCode(t *testing.T) {
	// collector key -> the file its emits live in, and the declaration to check.
	// Explicit because the names differ (conns/connections.go, rosusers).
	cases := []struct {
		key, file string
		want      Rooms
	}{
		{"bandwidth", "bandwidth.go", bandwidthRooms},
		{"bridges", "bridges.go", bridgesRooms},
		{"capsman", "capsman.go", capsmanRooms},
		{"dhcpNetworks", "dhcpnetworks.go", dhcpNetworksRooms},
		{"dns", "dns.go", dnsRooms},
		{"firewall", "firewall.go", firewallRooms},
		{"ifStatus", "ifstatus.go", ifStatusRooms},
		{"netwatch", "netwatch.go", netwatchRooms},
		{"packages", "packages.go", packagesRooms},
		{"ping", "ping.go", pingRooms},
		{"ppp", "ppp.go", pppRooms},
		{"queues", "queues.go", queuesRooms},
		{"rosusers", "rosusers.go", rosUsersRooms},
		{"routing", "routing.go", routingRooms},
		{"topology", "topology.go", topologyRooms},
		{"vlans", "vlans.go", vlansRooms},
		{"vpn", "vpn.go", vpnRooms},
		{"wan", "wan.go", wanRooms},
		{"wifi", "wifi.go", wifiRooms},
		{"wireless", "wireless.go", wirelessRooms},
	}

	emitLit := regexp.MustCompile(`emit\("([^"]*)"`)
	for _, c := range cases {
		src, err := os.ReadFile(c.file)
		if err != nil {
			t.Errorf("%s: %v", c.key, err)
			continue
		}
		// Every non-router-wide room this file emits to, from the literals.
		seen := map[string]bool{}
		for _, m := range emitLit.FindAllStringSubmatch(string(src), -1) {
			for _, r := range strings.Split(m[1], ",") {
				if r = strings.TrimSpace(r); r != "" {
					seen[r] = true
				}
			}
		}
		var got []string
		for r := range seen {
			got = append(got, r)
		}
		// The declaration is compared against the UNION for this key, because
		// `conns` and `ifStatus` emit to different subsets on different events.
		want := append([]string(nil), RoomsOf(c.key)...)
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: rooms.go declares %v, but %s still emits to %v.\n"+
				"The next commit points those emits at the declaration, so a mismatch "+
				"here would silently change who receives the payload.",
				c.key, want, c.file, got)
		}
	}
}

// TestThisBridgeIsStillNeeded fails when the literals are gone, which means the
// switch-over is complete and this whole file should be deleted.
//
// A bridge left standing after the crossing is worse than none: it reads as
// coverage while comparing a declaration against itself.
func TestThisBridgeIsStillNeeded(t *testing.T) {
	src, err := os.ReadFile("vpn.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `emit("page-vpn,dash-card-vpn"`) {
		t.Error("vpn.go no longer emits to a literal, so the switch-over has happened " +
			"and rooms_bridge_test.go has done its job. DELETE THIS FILE — a bridge " +
			"left standing compares the declarations against themselves.")
	}
}
