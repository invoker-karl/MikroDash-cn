package collect

// Interface status — the port of src/collectors/interfaceStatus.js.
//
// POLLED HERE, STREAMED THERE, AND THAT IS THE POINT. The Node collector holds
// three `=interval=N` channels open for its metadata (/interface, /ip/address,
// /interface/ethernet) and a fourth for rates. This reads the same four menus
// with ordinary prints on one connection. CLAUDE.md is explicit that "more
// efficient means fewer router channels, not faster payload assembly", and
// src/collection.js records the evidence: "the evidence in #104 points at
// concurrent open channels rather than data volume". Four fewer channels per
// router is the single biggest saving available in this port, and the payload is
// identical either way — a `=interval=N` print re-sends the same rows a plain
// print returns.
//
// The mechanism changed; the payload did not. That is the line CLAUDE.md draws.
//
// AND IT BUYS COMPLETENESS, WHICH IS NOT WHY IT WAS DONE. A `Do` returns a reply
// set the protocol itself terminates with `!done`; a persistent `=interval=N`
// stream hands over packets with no boundary the caller can see. So a delimited
// read is a COMPLETENESS GUARANTEE and a stream is not — which is a second and
// stronger reason for this departure than the channel count that motivated it.
//
// That is not hypothetical. The live app hit it as issue #119: its streaming
// version of this collector used a 300ms debounce to decide a cycle had ended,
// a debounce measures silence rather than completeness, and one mid-cycle gap
// installed a partial interface list as the whole truth — the traffic dropdown
// lost all but one interface on a CCR2004. This collector cannot have that bug,
// and not by foresight.
//
// IF THAT EVER CHANGES, THE FIX IS IN THE ADAPTER, NOT HERE. `internal/routeros`
// ends a Stream when go-routeros closes the channel, which it does on `!done` —
// so an `=interval=N` read would look like a stream that ended after one cycle.
// Adopting streaming for these menus means first surfacing the per-cycle `!done`
// as an event rather than an ending.
//
// RATES COME FROM THE ROUTER, NOT FROM DIFFERENCING BYTE COUNTERS.
// /interface/monitor-traffic reports rx-bits-per-second directly, and it can
// only be asked once the interface list is known — so the order within a tick is
// metadata first, rates second.
//
// ── THE FOUR READS ARE SPLIT, BECAUSE ONLY ONE OF THEM MOVES ────────────────
//
// This collector was four commands on every tick, and its tick is the fastest
// in the app. Measured against one idle CHR at the default cadence: 267 commands
// a minute reached the router, and 204 of them — 76% — were these four. That is
// what MikroDash costs a router when nobody is doing anything.
//
// Three of the four answer questions that do not change at that speed. An
// interface's name, type, comment, MAC, addresses and driver error counters do
// not move between one second and the next; asking 51 times a minute buys
// nothing. Only /interface/monitor-traffic answers a question whose whole value
// is that it is current.
//
// So the metadata reads are re-run every `metaTicks` polls and the rates read
// every poll, and a tick publishes the held metadata with the rates it just
// fetched. `metaTicks` targets `ifStatusMetaTarget` and is DERIVED from the poll
// interval rather than configured, so an operator who slows this collector down
// never gets metadata faster than rates, and one who speeds it up does not
// multiply the metadata cost with it.
//
// WHAT IS TRADED: `running`, `disabled` and the counters are up to
// `ifStatusMetaTarget` late. A link that drops is noticed within that window
// rather than within a poll — on the Interfaces page, in the sidebar badge and
// in the traffic picker. Rates, which are the thing anybody actually watches
// move, are unchanged.
//
// A POLL INTERVAL AT OR ABOVE THE TARGET COLLAPSES THIS BACK TO ONE LANE, which
// is not a coincidence to lean on: the frozen golden replay constructs this
// collector at 30s, so it takes that branch and reads all four menus on every
// tick exactly as before. The split is therefore NOT covered by the golden, and
// `TestIfStatusSplitsTheMetadataReads` is what covers it instead.

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"mikrodash/internal/roscache"
	"mikrodash/internal/routeros"
)

// ifStatusMetaTarget is how often the three metadata menus are re-read, and
// therefore how late a link state change can be. Five seconds is chosen against
// what the page does with it: an operator watching a port come up tolerates it,
// and the alternative — 51 reads a minute of rows that had not changed — is what
// this collector was doing.
const ifStatusMetaTarget = 5 * time.Second

// ifStatusMetaMaxTicks bounds the split from the other end. A very fast poll
// must not stretch the metadata over an unbounded number of ticks: at 500ms the
// target alone would ask for ten, and a page that has been running for a while
// on nine ticks of stale metadata is harder to reason about than one extra read.
const ifStatusMetaMaxTicks = 8

const ifCounterProps = "rx-byte,tx-byte,rx-error,tx-error,rx-drop,tx-drop," +
	"tx-queue-drop,link-downs,last-link-up-time"

var (
	ifStatusIfCmd = routeros.Cmd{Path: "/interface/print", Args: []string{
		"=.proplist=name,type,running,disabled,comment,mac-address," + ifCounterProps}}
	ifStatusAddrCmd = routeros.Cmd{Path: "/ip/address/print",
		Args: []string{"=.proplist=interface,address"}}
	ifStatusEthCmd = routeros.Cmd{Path: "/interface/ethernet/print", Args: []string{
		"=.proplist=name,rx-fcs-error,rx-align-error,rx-fragment,rx-overflow," +
			"rx-too-short,rx-too-long,tx-underrun,tx-late-collision,tx-excessive-collision"}}
)

var (
	ifErrFields  = []string{"rx-error", "tx-error"}
	ifDropFields = []string{"rx-drop", "tx-drop", "tx-queue-drop"}
	ethErrFields = []string{
		"rx-fcs-error", "rx-align-error", "rx-fragment", "rx-overflow",
		"rx-too-short", "rx-too-long",
		"tx-underrun", "tx-late-collision", "tx-excessive-collision",
	}
)

// Interface is one row of the interfaces payload.
type Interface struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Running  bool     `json:"running"`
	Disabled bool     `json:"disabled"`
	Comment  string   `json:"comment"`
	MacAddr  string   `json:"macAddr"`
	RxMbps   float64  `json:"rxMbps"`
	TxMbps   float64  `json:"txMbps"`
	IPs      []string `json:"ips"`
	// Cumulative counters. null means the interface does not report the counter
	// at all, which the list view renders as a dash rather than 0.
	RxBytes    *float64 `json:"rxBytes"`
	TxBytes    *float64 `json:"txBytes"`
	Errors     *float64 `json:"errors"`
	Drops      *float64 `json:"drops"`
	LinkDowns  *float64 `json:"linkDowns"`
	LastLinkUp string   `json:"lastLinkUp"`
	// Movement over the last window, null until a baseline exists.
	ErrorsDelta   *float64 `json:"errorsDelta"`
	DropsDelta    *float64 `json:"dropsDelta"`
	DeltaWindowMs *float64 `json:"deltaWindowMs"`
}

// IfStatusPayload is the ifstatus:update body.
type IfStatusPayload struct {
	TS         int64       `json:"ts"`
	RouterID   string      `json:"routerId"`
	Interfaces []Interface `json:"interfaces"`
}

// jsNum is `num()` on the Node side: undefined, null and "" are null; anything
// non-finite is null; everything else is the number.
func jsNum(row routeros.Reply, key string) *float64 {
	v, ok := row[key]
	if !ok || v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return nil
	}
	return &f
}

// sumCounters totals the fields a row actually reports. It stays null when the
// row reports NONE of them — "this interface has no error counters" is a
// different statement from "it has zero errors", and the page renders them
// differently.
func sumCounters(row routeros.Reply, fields []string) *float64 {
	var total *float64
	for _, f := range fields {
		n := jsNum(row, f)
		if n == nil {
			continue
		}
		if total == nil {
			z := 0.0
			total = &z
		}
		*total += *n
	}
	return total
}

// deltaOf is the movement between two readings, clamped at zero: a counter that
// went backwards was reset, and a negative delta would render as a fault that
// did not happen.
func deltaOf(prev, cur *float64) *float64 {
	if prev == nil || cur == nil {
		return nil
	}
	d := 0.0
	if *cur >= *prev {
		d = *cur - *prev
	}
	return &d
}

type counterSnap struct {
	errors *float64
	drops  *float64
	ts     time.Time
}

type counterDelta struct {
	errors   *float64
	drops    *float64
	windowMs float64
}

// IfStatus is the collector. It also serves as the RateSource for Bridges,
// VLANs and WAN — see Rates.
type IfStatus struct {
	ros      Reader
	emit     Emit
	routerID string
	pollMs   *pollInterval
	// cache coalesces reads shared with another collector. Nil outside a live
	// session, which is every test — see collect/cache.go.
	cache *roscache.Cache

	poll *pollLoop

	mu    sync.Mutex
	prev  map[string]counterSnap
	delta map[string]counterDelta

	// The metadata half, held between refreshes. `base` is the payload's
	// interface list with every field EXCEPT the rates filled in, so a tick that
	// only fetched rates still publishes a complete row. `ifRows` is kept beside
	// it because the rates read needs the raw name/disabled columns.
	//
	// base nil means "metadata has never been read", which is also what a
	// reconnect leaves behind; metaIn counts polls down to the next refresh.
	base   []Interface
	ifRows []routeros.Reply
	metaIn int

	last       *IfStatusPayload
	lastErr    string
	lastFp     string
	lastEmitAt time.Time
}

// NewIfStatus builds the collector.
func NewIfStatus(ros Reader, emit Emit, routerID string, pollMs int) *IfStatus {
	s := &IfStatus{
		ros: ros, emit: emit, routerID: routerID,
		// Node calls clampPoll(pollMs, 5000) here and takes the DEFAULT bounds,
		// which floor at 500ms — not the 2000 the other collectors pass
		// explicitly. A 1s setting is honoured there and would have been floored
		// to 2s here, so the port would have polled at half the rate the
		// operator asked for, on the one collector whose whole job is rates.
		pollMs: newPollInterval(clampPoll(pollMs, 5000, 500, 60000)),
		prev:   map[string]counterSnap{},
		delta:  map[string]counterDelta{},
	}
	s.poll = newPollLoop(func() { s.Tick() }, func() time.Duration {
		return s.pollMs.duration()
	})
	return s
}

func (s *IfStatus) read(cmd routeros.Cmd) []routeros.Reply {
	rows, err := s.ros.Do(cmd)
	if err != nil {
		if !menuMissing(err) {
			s.lastErr = err.Error()
		}
		return nil
	}
	out := make([]routeros.Reply, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// rates asks the router for throughput on the interfaces it just learned about.
//
// Disabled interfaces are excluded — and the filter is written the long way on
// purpose. RouterOS sends `disabled` as the STRING "false", which is truthy in
// JavaScript, so the Node side's first attempt at `!iface.disabled` excluded
// EVERY interface and rates sat at zero forever. Go has no such trap, but the
// comparison is spelled out so the next reader does not "simplify" it back.
func (s *IfStatus) rates(ifaces []routeros.Reply) map[string]Rate {
	names := make([]string, 0, len(ifaces))
	for _, i := range ifaces {
		if i["name"] == "" || i["disabled"] == "true" {
			continue
		}
		names = append(names, i["name"])
	}
	out := map[string]Rate{}
	if len(names) == 0 {
		return out
	}
	rows, err := s.ros.Do(routeros.Cmd{Path: "/interface/monitor-traffic", Args: []string{
		"=interface=" + strings.Join(names, ","),
		"=once=",
		"=.proplist=name,rx-bits-per-second,tx-bits-per-second",
	}})
	if err != nil {
		s.lastErr = err.Error()
		return out
	}
	for _, r := range rows {
		if r["name"] == "" {
			continue
		}
		rx, tx := bpsToMbps(r["rx-bits-per-second"]), bpsToMbps(r["tx-bits-per-second"])
		out[r["name"]] = Rate{RxMbps: &rx, TxMbps: &tx}
	}
	return out
}

// bpsToMbps matches parseBps + bpsToMbps: bits per second to Mbps, rounded to
// three decimals, which is the precision the payload has always carried.
func bpsToMbps(v string) float64 {
	if v == "" || v == "0" {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0
	}
	return round3(f / 1e6)
}

func round3(f float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 3, 64), 64)
	return r
}

// metaTicks is how many polls apart the three metadata reads are.
//
// DERIVED, NOT CONFIGURED. There is no second interval for an operator to set:
// the metadata cadence is the poll interval rounded up to the target, so the two
// can never be tuned into disagreeing. Note what the bounds mean — a poll at or
// above the target returns 1 and every tick reads all four menus, which is
// exactly the behaviour this collector had before the split.
func (s *IfStatus) metaTicks() int {
	p := s.pollMs.duration()
	if p <= 0 {
		return 1
	}
	n := int((ifStatusMetaTarget + p/2) / p) // nearest, not floor
	if n < 1 {
		return 1
	}
	if n > ifStatusMetaMaxTicks {
		return ifStatusMetaMaxTicks
	}
	return n
}

// Tick publishes a payload every poll. It fetches the RATES every time and the
// three metadata menus only every metaTicks — see the split note in the header.
func (s *IfStatus) Tick() {
	if !s.ros.Connected() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.base == nil || s.metaIn <= 0 {
		s.refreshMeta()
		s.metaIn = s.metaTicks()
	}
	s.metaIn--

	if len(s.base) == 0 {
		return // nothing to build from; do not publish an empty interface list
	}

	// The fast half, and on most ticks the ONLY command this collector issues.
	rateBy := s.rates(s.ifRows)

	// Copied rather than written through, because `base` outlives the tick and
	// a rate written into it would still be there when the router stopped
	// reporting one — the interface would hold its last speed for ever.
	interfaces := make([]Interface, len(s.base))
	copy(interfaces, s.base)
	for i := range interfaces {
		rt, ok := rateBy[interfaces[i].Name]
		if !ok {
			continue
		}
		if rt.RxMbps != nil {
			interfaces[i].RxMbps = *rt.RxMbps
		}
		if rt.TxMbps != nil {
			interfaces[i].TxMbps = *rt.TxMbps
		}
	}

	now := time.Now()
	payload := &IfStatusPayload{TS: now.UnixMilli(), RouterID: s.routerID, Interfaces: interfaces}
	s.last = payload

	// Byte totals are deliberately absent: they creep up even on an idle link
	// (broadcast traffic), so including them would defeat the suppression this
	// exists for. Errors, drops and flap counts are in — they hold steady on a
	// healthy link, so any movement is worth pushing at once. type, comment and
	// MAC are in for the opposite reason: they never move on their own, so they
	// cost nothing, and leaving them out meant an edit to one never reached an
	// open page.
	// A HEARTBEAT EVEN WHEN NOTHING MOVED. Suppressing an identical payload for
	// ever leaves the browser unable to tell an idle interface from a dead
	// collector, and the page's staleness overlay fires on a link that is
	// simply quiet. Sixty seconds, matching the original.
	fp := ifStatusFingerprint(interfaces)
	if fp == s.lastFp && now.Sub(s.lastEmitAt) < ifStatusHeartbeat {
		return
	}
	s.lastFp, s.lastEmitAt = fp, now

	// SPLIT DELIVERY, and the split is an authorisation boundary.
	//
	// The full payload carries per-interface rates, IP addresses and MAC
	// addresses, so it reaches only the pages that render them: Interfaces,
	// Topology (link rates) and the dashboard's ports card. One copy per
	// viewer — a viewer can be in two of those rooms.
	//
	// The router-wide half carries NAMES AND UP/DOWN ONLY. That is exactly what
	// the traffic chart's interface picker and the sidebar badge need, and they
	// are chrome on every page — so it must not be withheld from a viewer who
	// has opened none of those three, and it must not disclose anything a denied
	// page would have shown.
	s.emit("page-interfaces,page-network-topology,dash-card-physports", "ifstatus:update", payload)

	s.emit("", "ifstatus:names", NamesOf(payload))
}

// refreshMeta re-reads the three metadata menus and rebuilds the interface list
// everything except the rates comes from.
//
// A FAILED READ KEEPS THE PREVIOUS METADATA rather than blanking the list. That
// is a change from the pre-split collector, which skipped the whole tick: rates
// now keep flowing over one interval of slightly older metadata instead of the
// page freezing entirely. The first read is the exception — there is nothing to
// keep, and `base` staying nil is what makes Tick decline to publish.
func (s *IfStatus) refreshMeta() {
	// THROUGH THE CACHE: `wan` reads this same menu with a proplist that is a
	// strict subset of ours, so once either has fetched, the other costs nothing.
	// The TTL offered is this collector's METADATA interval, not its poll — the
	// cache keeps the shortest any consumer asks for, so offering the poll here
	// would pin the entry to a freshness this half no longer needs.
	ifRows, _ := readVia(s.cache, s.ros, ifStatusIfCmd, time.Duration(s.metaTicks())*s.pollMs.duration())
	addrRows := s.read(ifStatusAddrCmd)
	ethRows := s.read(ifStatusEthCmd)
	if len(ifRows) == 0 {
		return
	}

	addrs := map[string][]string{}
	for _, a := range addrRows {
		if a["interface"] == "" {
			continue
		}
		addrs[a["interface"]] = append(addrs[a["interface"]], a["address"])
	}
	eth := map[string]routeros.Reply{}
	for _, e := range ethRows {
		if e["name"] != "" {
			eth[e["name"]] = e
		}
	}

	now := time.Now()
	snap := map[string]counterSnap{}
	delta := map[string]counterDelta{}
	base := make([]Interface, 0, len(ifRows))

	for _, r := range ifRows {
		name := r["name"]
		// Errors are the interface's own plus the ethernet driver's, when the
		// interface has an ethernet row. Either may be absent; null only when
		// BOTH are.
		errs := sumCounters(r, ifErrFields)
		if e, ok := eth[name]; ok {
			if phy := sumCounters(e, ethErrFields); phy != nil {
				if errs == nil {
					z := 0.0
					errs = &z
				}
				v := *errs + *phy
				errs = &v
			}
		}
		drops := sumCounters(r, ifDropFields)

		// The delta window is now the METADATA cadence, which is what it always
		// meant to be: differencing two readings of the same rows would report a
		// zero delta over a window no counter had a chance to move in.
		snap[name] = counterSnap{errors: errs, drops: drops, ts: now}
		if p, ok := s.prev[name]; ok {
			de, dd := deltaOf(p.errors, errs), deltaOf(p.drops, drops)
			if de != nil || dd != nil {
				delta[name] = counterDelta{errors: de, drops: dd,
					windowMs: float64(now.Sub(p.ts).Milliseconds())}
			}
		}

		typ := r["type"]
		if typ == "" {
			typ = "ether"
		}
		ips := addrs[name]
		if ips == nil {
			ips = []string{}
		}
		iface := Interface{
			Name: name, Type: typ,
			Running: r["running"] == "true", Disabled: r["disabled"] == "true",
			Comment: r["comment"], MacAddr: r["mac-address"],
			IPs:        ips,
			RxBytes:    jsNum(r, "rx-byte"),
			TxBytes:    jsNum(r, "tx-byte"),
			Errors:     errs,
			Drops:      drops,
			LinkDowns:  jsNum(r, "link-downs"),
			LastLinkUp: r["last-link-up-time"],
		}
		if d, ok := delta[name]; ok {
			iface.ErrorsDelta, iface.DropsDelta = d.errors, d.drops
			w := d.windowMs
			iface.DeltaWindowMs = &w
		}
		base = append(base, iface)
	}

	// Replaced rather than merged, so a renamed interface does not leave a stale
	// delta behind for a name that later gets reused.
	s.prev, s.delta = snap, delta
	s.ifRows, s.base = ifRows, base
}

// NamesOf reduces a full interface payload to the names-and-state one.
//
// EXTRACTED so the emit above and the handshake replay in `ws.go` share one
// implementation. It was inline, and the replay — added when
// The initial-state audit found that a viewer attaching to a running
// session never received this — would otherwise have been a second copy of the
// same three-field projection, which is a defect with a delay fuse.
func NamesOf(payload *IfStatusPayload) *IfNamesPayload {
	if payload == nil {
		return nil
	}
	names := make([]IfName, 0, len(payload.Interfaces))
	for _, i := range payload.Interfaces {
		names = append(names, IfName{Name: i.Name, Running: i.Running, Disabled: i.Disabled})
	}
	return &IfNamesPayload{TS: payload.TS, Total: len(payload.Interfaces), Interfaces: names}
}

// ifStatusHeartbeat is how long an unchanged payload may be suppressed.
const ifStatusHeartbeat = 60 * time.Second

// IfName is one interface as the chrome sees it: enough to fill a picker and a
// badge, and nothing a page grant would have gated.
type IfName struct {
	Name     string `json:"name"`
	Running  bool   `json:"running"`
	Disabled bool   `json:"disabled"`
}

type IfNamesPayload struct {
	TS         int64    `json:"ts"`
	Total      int      `json:"total"`
	Interfaces []IfName `json:"interfaces"`
}

func round2(f float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', 2, 64), 64)
	return r
}

// Rates makes this collector the RateSource for Bridges, VLANs and WAN.
//
// The bool is "has this reported at all". Those pages render an em dash for
// "not reported" and 0.00 for idle, and conflating them makes a collector that
// has not started look like a quiet network.
func (s *IfStatus) Rates() (map[string]Rate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		return nil, false
	}
	out := make(map[string]Rate, len(s.last.Interfaces))
	for _, i := range s.last.Interfaces {
		rx, tx := i.RxMbps, i.TxMbps
		out[i.Name] = Rate{RxMbps: &rx, TxMbps: &tx}
	}
	return out, true
}

// Last is the most recent payload, replayed on page:focus.
func (s *IfStatus) Last() *IfStatusPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *IfStatus) Start() {
	if s.ros.Connected() {
		s.Tick()
	}
	s.poll.start()
}

func (s *IfStatus) Reconnected() {
	s.poll.stop()
	s.mu.Lock()
	s.lastFp, s.lastEmitAt = "", time.Time{}
	// A reconnect may be a different router, and differencing a counter across
	// that boundary would report movement that never happened.
	s.prev = map[string]counterSnap{}
	s.delta = map[string]counterDelta{}
	// And the metadata, for the same reason: it describes the interfaces of a
	// connection that is gone. Nil is what makes the next tick re-read.
	s.base, s.ifRows, s.metaIn = nil, nil, 0
	s.mu.Unlock()
	s.Tick()
	s.poll.start()
}

// Suspend stops the poll, and ARMS the next metadata read.
//
// Without that, a page left blurred for an hour resumes by publishing an hour
// old interface list with fresh rates on it, and keeps doing so until the
// countdown that was mid-flight when it stopped runs out. The rows are still
// worth keeping — they are the right shape and mostly still true — so this
// re-reads them on the first tick back rather than blanking the page.
func (s *IfStatus) Suspend() {
	s.poll.stop()
	s.mu.Lock()
	s.metaIn = 0
	s.mu.Unlock()
}

func (s *IfStatus) Resume() {
	if s.ros.Connected() {
		s.poll.start()
	}
}

func (s *IfStatus) Stop() {
	s.poll.stop()
	s.mu.Lock()
	s.lastFp, s.lastEmitAt = "", time.Time{}
	s.mu.Unlock()
}

// ifStatusFingerprint is EXTRACTED so it can be gated. It was inline, which is
// why the rule it embodies — every field the page renders belongs here — had no
// test: there was nothing to call. See fingerprint_test.go.
func ifStatusFingerprint(interfaces []Interface) string {
	type fpRow struct {
		N, T, C, M string
		R, D       bool
		Rx, Tx     float64
		IPs        []string
		E, Dr, Ld  *float64
	}
	rows := make([]fpRow, 0, len(interfaces))
	for _, i := range interfaces {
		rows = append(rows, fpRow{i.Name, i.Type, i.Comment, i.MacAddr, i.Running, i.Disabled,
			round2(i.RxMbps), round2(i.TxMbps), i.IPs, i.Errors, i.Drops, i.LinkDowns})
	}
	b, _ := json.Marshal(rows)
	return string(b)
}

// SetPollMs applies a new poll period to a running collector.
// See `System.SetPollMs` for why both halves are needed.
func (i *IfStatus) SetPollMs(ms int) {
	i.pollMs.set(ms)
	i.poll.retime()
}

// UseCache routes this collector's shareable reads through a per-router cache.
// Set once, before Start; nil leaves every read direct.
func (s *IfStatus) UseCache(c *roscache.Cache) { s.cache = c }
