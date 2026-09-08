package collect

// Where every collector's payload goes, declared once.
//
// ── WHY THIS FILE EXISTS ────────────────────────────────────────────────────
//
// A collector's audience was written down twice: as the first argument to its
// `emit`, and again by hand in `internal/server/ws.go`, where `pageBlur` decides
// whether anybody is still watching before it suspends the collector.
//
// Two statements of one fact, and they have disagreed FIVE times — dhcpNetworks,
// bandwidth, vpn, firewall, and routing on 2026-08-31. Each time the symptom was
// the same and silent: a dashboard card stopped updating for anybody who had
// visited the owning page and left, because the blur suspended a collector that
// was still feeding a card the guard did not know about.
//
// This is phase 4.2's core, reduced to the part that was actually buying
// something. A view declares its rooms; everything else reads the declaration.
//
// ── THE PRECEDENT WAS ALREADY HERE ──────────────────────────────────────────
//
// `logs` and `talkers` already emitted to a named constant rather than a
// literal, and the verify gates already carried an exemption for it. So this is
// the existing pattern applied to the other twenty-three, not a new one.
//
// ── ROOMS ARE PER-EVENT, NOT PER-COLLECTOR ──────────────────────────────────
//
// Two collectors emit to different audiences for different events, and flattening
// that would be wrong rather than untidy:
//
//	conns     `conn:update` reaches the page AND the dashboard card; the two
//	          detail events reach only the page, because only the page renders them.
//	ifStatus  its payload reaches three rooms; `ifstatus:names` is router-wide.
//
// So each set is named for what it is. `Union` is what the blur guard wants —
// everything this collector feeds — and it is derived rather than declared, so
// it cannot disagree with the parts.

import "strings"

// Rooms is one audience. The zero value is the ROUTER-WIDE room: every viewer of
// this router, whatever page they are on.
//
// It is a named type rather than a bare []string so that `Join` lives with it and
// nobody re-invents the comma.
type Rooms []string

// Join renders the room list as `emit` takes it.
func (r Rooms) Join() string { return strings.Join(r, ",") }

// routerWide is an emit with no room: everyone watching this router.
//
// NOT A ROOM, and the blur guard must never wait on it — every viewer occupies
// it, so guarding on it would mean never suspending at all. `dhcpNetworks` is the
// case that proves it and `ws.go` carries the reasoning at its call site.
var routerWide = Rooms{}

// ── THE DECLARATIONS ────────────────────────────────────────────────────────
//
// One line per audience. A page room is `page-<page key>`; a dashboard card room
// is `dash-card-<name>`, and the two name spaces are separate on purpose — a card
// can outlive the page that owns it.
var (
	bandwidthRooms    = Rooms{"page-bandwidth", "dash-card-bandwidth"}
	bridgesRooms      = Rooms{"page-bridges"}
	capsmanRooms      = Rooms{"page-capsman"}
	dhcpNetworksRooms = Rooms{"page-dhcp", "dash-card-network"}
	connsRooms        = Rooms{"page-connections", "dash-card-connections"}
	// connsDetailRooms: the country and source breakdowns are rendered by the
	// page and by nothing else, so sending them to the card would be traffic
	// nobody reads.
	connsDetailRooms = Rooms{"page-connections"}
	dnsRooms         = Rooms{"page-dns"}
	netwatchRooms    = Rooms{"page-dashboard"}
	firewallRooms    = Rooms{"page-firewall", "dash-card-firewall"}
	ifStatusRooms    = Rooms{"page-interfaces", "page-network-topology", "dash-card-physports"}
	logsRooms        = Rooms{"page-logs", "dash-card-logs"}
	pppRooms         = Rooms{"page-ppp"}
	packagesRooms    = Rooms{"page-packages"}
	pingRooms        = Rooms{"page-dashboard"}
	rosUsersRooms    = Rooms{"page-users"}
	queuesRooms      = Rooms{"page-queues"}
	routingRooms     = Rooms{"page-routing", "page-dashboard"}
	talkersRooms     = Rooms{"page-dashboard"}
	topologyRooms    = Rooms{"page-network-topology"}
	vlansRooms       = Rooms{"page-vlans"}
	vpnRooms         = Rooms{"page-vpn", "dash-card-vpn"}
	wanRooms         = Rooms{"page-wan"}
	wifiRooms        = Rooms{"page-wifi-networks"}
	wirelessRooms    = Rooms{"page-wifi-clients", "dash-card-wireless"}
)

// RoomsOf is every room a collector feeds, for `internal/server`'s blur guard.
//
// ── DERIVED, SO IT CANNOT DISAGREE WITH THE PARTS ───────────────────────────
//
// `conns` is why this is a union rather than a single declaration: it feeds the
// card on one event and the page on two others, and the guard needs to know
// about both. Writing the union out by hand would put the fact back in two
// places, which is what this file exists to stop.
//
// The router-wide room is deliberately absent from every entry. Three collectors
// emit to it (`system`, `dhcpLeases`, and `traffic`'s health and WAN chips) and
// two more do so alongside a page room; none of that is guardable.
//
// An unknown key returns nil, and the caller treats nil as "no rooms to wait on"
// — which suspends. `internal/verify` asserts every disableable collector has an
// entry, so nil means a collector that was never meant to be guarded.
func RoomsOf(key string) Rooms {
	switch key {
	case "bandwidth":
		return bandwidthRooms
	case "bridges":
		return bridgesRooms
	case "capsman":
		return capsmanRooms
	case "conns":
		return union(connsRooms, connsDetailRooms)
	case "dhcpNetworks":
		return dhcpNetworksRooms
	case "dns":
		return dnsRooms
	case "firewall":
		return firewallRooms
	case "ifStatus":
		return ifStatusRooms
	case "logs":
		return logsRooms
	case "netwatch":
		return netwatchRooms
	case "packages":
		return packagesRooms
	case "ping":
		return pingRooms
	case "ppp":
		return pppRooms
	case "queues":
		return queuesRooms
	case "rosusers":
		return rosUsersRooms
	case "routing":
		return routingRooms
	case "talkers":
		return talkersRooms
	case "topology":
		return topologyRooms
	case "vlans":
		return vlansRooms
	case "vpn":
		return vpnRooms
	case "wan":
		return wanRooms
	case "wifi":
		return wifiRooms
	case "wireless":
		return wirelessRooms
	}
	return nil
}

// keepAliveFor is rooms where a collector must keep RUNNING for another
// collector's sake, and which are not part of its own audience.
//
// ── AN AUDIENCE AND A DEPENDENCY ARE DIFFERENT THINGS ───────────────────────
//
// `conns` emits to the Connections page and its dashboard card, and to nothing
// else. But `bandwidth` reads the connection table `conns` deposits in
// `ConnTable`, so suspending `conns` while somebody is on the BANDWIDTH page
// starves a page `conns` never sends to.
//
// That is why `suspendConnsIfIdle` waited on `page-bandwidth`, and it is the one
// entry here. Modelling it as an audience would have been wrong -- nothing is
// ever emitted there by this collector -- and dropping it in the move to
// declarations would have starved the Bandwidth page silently.
//
// ── IT MAY NOW BE REMOVABLE, AND IS DELIBERATELY NOT REMOVED ────────────────
//
// Since 3.2c `bandwidth` subscribes to `/ip/firewall/connection` itself and takes
// its rows from the scheduler, so on a live session it no longer needs
// `ConnTable` at all. The dependency survives only on the POLLED path, which a
// Session never takes. Removing this is therefore probably safe and is a
// behaviour change resting on that "probably", so it is recorded rather than
// made in a refactor whose whole point is to change nothing.
var keepAliveFor = map[string]Rooms{
	"conns": {"page-bandwidth"},
}

// Others is the rooms a collector feeds APART from one page: what `pageBlur`
// must find empty before it may suspend.
//
// This is the whole calculation the seven guard call sites used to spell out by
// hand, and getting it wrong in either direction is a real defect: too few rooms
// starves a card, too many means the collector never suspends and keeps asking a
// router nobody is watching.
func Others(key, pageKey string) []string {
	var out []string
	for _, r := range union(RoomsOf(key), keepAliveFor[key]) {
		if r == "" || r == "page-"+pageKey {
			continue
		}
		out = append(out, r)
	}
	return out
}

func union(sets ...Rooms) Rooms {
	seen := map[string]bool{}
	var out Rooms
	for _, s := range sets {
		for _, r := range s {
			if r == "" || seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}
