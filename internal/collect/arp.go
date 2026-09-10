package collect

// ARP collector — the port of src/collectors/arp.js.
//
//	/ip/arp/print   address, mac-address, interface
//
// ── IT EMITS NOTHING, AND THAT IS THE WHOLE SHAPE ───────────────────────────
//
// Every other collector answers a page. This one answers OTHER COLLECTORS: the
// ARP table is the only place the router says which MAC is behind which IP, and
// four consumers need that join to put a name or an address on a device.
//
//	connections  lease-by-IP misses -> ARP gives the MAC -> lease-by-MAC names it
//	bandwidth    the same chain, on the same table
//	wireless     a registration row carries a MAC and no address; ARP has the
//	             address, which is the `ip` the WiFi Clients page renders
//	topology     an MNDP neighbour often carries no address at all
//
// So it takes no `Emit`. Taking one and never calling it would read as a
// collector whose payload nobody happens to render, which is a different and
// wrong thing: `internal/collect/rooms.go` declares no audience for `arp`
// because it HAS none.
//
// ── WHY IT WAS ABSENT, AND WHY THAT REASON EXPIRED ─────────────────────────
//
// `internal/collect/retime_test.go` recorded the decision: "this port has NO ARP
// collector... inventing one so this table could be complete would be a
// collector with no caller." That was true when written and stopped being true
// without anything failing — `topology.go` DECLARES `ARPIP func(mac) string` and
// uses it at two sites, and nothing ever set it, so the fallback was dead code
// behind a nil check. Measured 2026-09-10 on the live fleet: 26 of 26 WiFi
// clients had no address, because `parseWirelessClient` was passed a literal
// empty string where the live collector passed an ARP lookup.
//
// ── SET A: A TABLE, AND A CACHEABLE ONE ─────────────────────────────────────
//
// Rows are successive readings of a keyed value — an IP's MAC — so this is an
// ordinary subscribed menu that a channel may back, unlike `ping` and `logs`
// whose rows are distinct events. The live collector used `/ip/arp/listen` for
// the same reason and called the table "stable, low-churn".

import (
	"strings"
	"sync"

	"mikrodash/internal/roscache"
	"mikrodash/internal/routeros"
)

var arpCmd = routeros.Cmd{Path: "/ip/arp/print", Args: []string{
	"=.proplist=address,mac-address,interface",
}}

// ARPEntry is one row, reduced to the join it exists for.
type ARPEntry struct {
	IP    string
	MAC   string
	Iface string
}

// ARPIndex is the derivation: the table as its two lookups.
//
// BOTH DIRECTIONS ARE BUILT, because the four consumers split evenly between
// them — `connections` and `bandwidth` ask IP→MAC, `wireless` and `topology` ask
// MAC→IP — and building one and scanning for the other would make one consumer
// linear in the table on every row it renders.
type ARPIndex struct {
	ByIP  map[string]ARPEntry
	ByMAC map[string]ARPEntry
}

// BuildARP is the derivation: rows in, both indexes out.
//
// ── THE RULES ARE THE LIVE ONES, AND EACH IS LOAD-BEARING ──────────────────
//
// `address` OR `active-address`: the live `_applyEntry` reads the first and
// falls back to the second, because a published or DHCP-sourced row can carry
// only the latter.
//
// A ROW NEEDS BOTH HALVES. An entry with an IP and no MAC — RouterOS emits one
// for a failed resolution, and the live fleet had exactly that at 172.16.0.251 —
// joins nothing in either direction, and indexing it would put an empty MAC in
// `ByMAC` where it would answer for every nameless lookup.
//
// MACs are keyed UPPER-CASE. RouterOS answers upper here and the registration
// tables agree, but the lease table's `active-mac-address` has been seen both
// ways, and a case-sensitive map is a join that silently finds nothing.
//
// LAST ROW WINS on a duplicate key, which is the live Map's behaviour: two IPs
// on one MAC is ordinary (a device that moved subnet), and the later row is the
// current one.
func BuildARP(rows []routeros.Reply) *ARPIndex {
	ix := &ARPIndex{
		ByIP:  make(map[string]ARPEntry, len(rows)),
		ByMAC: make(map[string]ARPEntry, len(rows)),
	}
	for _, r := range rows {
		ip := firstNonEmptyStr(r["address"], r["active-address"])
		mac := strings.ToUpper(strings.TrimSpace(r["mac-address"]))
		if ip == "" || mac == "" {
			continue
		}
		e := ARPEntry{IP: ip, MAC: mac, Iface: r["interface"]}
		ix.ByIP[ip] = e
		ix.ByMAC[mac] = e
	}
	return ix
}

// ARPByIP answers "which MAC is behind this address". Consumed by `connections`
// and `bandwidth`, each to reach a DHCP lease the address alone does not find.
type ARPByIP interface {
	MACForIP(ip string) (mac, iface string)
}

// ARPByMAC answers "which address is this MAC using". Consumed by `wireless`,
// where a registration row has no address at all, and by `topology`, where an
// MNDP neighbour often has none.
type ARPByMAC interface {
	IPForMAC(mac string) string
}

// ARP is the collector.
type ARP struct {
	ros    Reader
	poll   *pollLoop
	sched  scheduled
	pollMs *pollInterval

	mu sync.Mutex
	ix *ARPIndex
}

// NewARP builds the collector. No `Emit`: see the header.
func NewARP(ros Reader, pollMs int) *ARP {
	a := &ARP{ros: ros, pollMs: newPollInterval(clampPoll(pollMs, 30000, 5000, 300000))}
	a.poll = newPollLoop(func() { a.Tick() }, a.pollMs.duration)
	a.sched = scheduled{
		loop: a.poll, menu: arpCmd.Path, fields: fieldsOf(arpCmd), apply: a.apply,
		cadence: a.pollMs.duration,
	}
	return a
}

// UseCache moves this collector onto the router's scheduler.
func (a *ARP) UseCache(c *roscache.Cache) { a.sched.useCache(c) }

func (a *ARP) Tick() {
	if !a.ros.Connected() {
		return
	}
	a.apply(a.ros.Do(arpCmd))
}

// apply is what the scheduler calls with the table.
//
// NO FINGERPRINT AND NO EMIT, so there is nothing to suppress: the index is
// replaced wholesale and the next consumer to look sees the new one. The live
// collector updated its Maps in place "so callers always see the latest data
// without any coordination overhead"; replacing the whole index is the same
// property with one lock instead of two maps mutated under none.
func (a *ARP) apply(rows []routeros.Reply, err error) {
	if err != nil {
		return
	}
	ix := BuildARP(rows)
	a.mu.Lock()
	a.ix = ix
	a.mu.Unlock()
}

// Last is the current index, or nil before the first read.
//
// Exported because the dormancy target table needs a `last` closure like every
// other collector's, and because a nil index is how "this collector has not
// reported" is expressed everywhere else.
func (a *ARP) Last() *ARPIndex {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ix
}

// MACForIP implements ARPByIP.
func (a *ARP) MACForIP(ip string) (string, string) {
	a.mu.Lock()
	ix := a.ix
	a.mu.Unlock()
	if ix == nil {
		return "", ""
	}
	e := ix.ByIP[ip]
	return e.MAC, e.Iface
}

// IPForMAC implements ARPByMAC. The lookup is case-insensitive for the reason
// `BuildARP` records: the tables this is joined against do not agree on case.
func (a *ARP) IPForMAC(mac string) string {
	a.mu.Lock()
	ix := a.ix
	a.mu.Unlock()
	if ix == nil {
		return ""
	}
	return ix.ByMAC[strings.ToUpper(strings.TrimSpace(mac))].IP
}

func (a *ARP) Start() {
	if !a.sched.scheduling() {
		a.Tick()
	}
	a.sched.begin()
}

func (a *ARP) Reconnected() {
	a.sched.end()
	// THE INDEX IS DROPPED. A reconnect may be to a router that has rebooted, and
	// a stale ARP table names devices at addresses they no longer hold — which is
	// worse than naming none, because a wrong name looks like a right one.
	a.mu.Lock()
	a.ix = nil
	a.mu.Unlock()
	if !a.sched.scheduling() {
		a.Tick()
	}
	a.sched.begin()
}

func (a *ARP) Suspend() { a.sched.end() }
func (a *ARP) Resume()  { a.sched.begin() }

func (a *ARP) Stop() { a.sched.end() }

// SetPollMs re-tunes the interval. `pollArp` has been a persisted, validated,
// writable setting since the port with nothing reading it; this is what makes it
// mean something.
func (a *ARP) SetPollMs(ms int) {
	a.pollMs.set(clampPoll(ms, 30000, 5000, 300000))
	// The loop is retimed and the SUBSCRIPTION is not, which is the same shape
	// every other re-tunable collector has: `scheduled.cadence` is a closure over
	// `pollMs`, so the scheduler reads the new value on its next pass without
	// being told.
	a.poll.retime()
}
