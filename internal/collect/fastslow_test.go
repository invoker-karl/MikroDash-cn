package collect

import (
	"strings"
	"testing"
	"time"

	"mikrodash/internal/routeros"
)

// ── THE SPLIT IS NOT COVERED BY THE GOLDEN, SO IT IS COVERED HERE ────────────
//
// `fixture_test.go` builds this collector at a 30s poll, which is at or above
// `ifStatusMetaTarget`, so `metaTicks` returns 1 and the replay reads all four
// menus on every tick exactly as it did before the split. That is why the golden
// stayed green through this change — a fact worth stating rather than enjoying.
//
// These tests drive the branch the golden cannot reach: a fast poll, where the
// three metadata menus must fall behind the rates read and the payload must stay
// complete anyway.

type splitReader struct {
	byMenu map[string]int
}

func (r *splitReader) Connected() bool { return true }

func (r *splitReader) Do(cmd routeros.Cmd) ([]routeros.Reply, error) {
	r.byMenu[cmd.Path]++
	switch cmd.Path {
	case "/interface/print":
		return []routeros.Reply{{
			"name": "ether1", "type": "ether", "running": "true", "disabled": "false",
			"mac-address": "02:00:00:00:00:01", "rx-byte": "100", "tx-byte": "200",
			"rx-error": "1", "tx-error": "0", "rx-drop": "0", "tx-drop": "0",
			"tx-queue-drop": "0", "link-downs": "0",
		}}, nil
	case "/ip/address/print":
		return []routeros.Reply{{"interface": "ether1", "address": "198.51.100.1/24"}}, nil
	case "/interface/ethernet/print":
		return []routeros.Reply{{"name": "ether1", "rx-fcs-error": "0"}}, nil
	case "/interface/monitor-traffic":
		// A different value every call, so a suppressed emit is distinguishable
		// from a stale one.
		n := r.byMenu[cmd.Path]
		return []routeros.Reply{{
			"name":               "ether1",
			"rx-bits-per-second": strings.Repeat("1", n) + "000000",
			"tx-bits-per-second": "2000000",
		}}, nil
	}
	return nil, nil
}

// TestIfStatusSplitsTheMetadataReads is the whole point of the split: at a fast
// poll the rates read runs on every tick and the other three do not.
func TestIfStatusSplitsTheMetadataReads(t *testing.T) {
	r := &splitReader{byMenu: map[string]int{}}
	c := NewIfStatus(r, func(string, string, any) {}, "r1", 1000)

	want := c.metaTicks()
	if want < 2 {
		t.Fatalf("a 1s poll must split; metaTicks = %d", want)
	}

	// Enough ticks to cross the boundary twice, whatever the target is set to.
	ticks := 2*want + 1
	for i := 0; i < ticks; i++ {
		c.Tick()
	}

	if got := r.byMenu["/interface/monitor-traffic"]; got != ticks {
		t.Errorf("rates read %d times, want one per tick (%d)", got, ticks)
	}
	wantMeta := 1 + (ticks-1)/want // the first tick, then one every `want`
	for _, menu := range []string{"/interface/print", "/ip/address/print", "/interface/ethernet/print"} {
		if got := r.byMenu[menu]; got != wantMeta {
			t.Errorf("%s read %d times over %d ticks, want %d", menu, got, ticks, wantMeta)
		}
	}
}

// TestIfStatusPublishesFullRowsBetweenMetadataReads pins the property that makes
// the split safe: a tick that fetched nothing but rates still emits an interface
// carrying its addresses, MAC and counters, and carries the NEW rate.
func TestIfStatusPublishesFullRowsBetweenMetadataReads(t *testing.T) {
	r := &splitReader{byMenu: map[string]int{}}
	c := NewIfStatus(r, func(string, string, any) {}, "r1", 1000)

	c.Tick() // reads everything
	first := c.Last()
	c.Tick() // rates only
	second := c.Last()

	if len(second.Interfaces) != 1 {
		t.Fatalf("second tick published %d interfaces, want 1", len(second.Interfaces))
	}
	got := second.Interfaces[0]
	if len(got.IPs) != 1 || got.IPs[0] != "198.51.100.1/24" {
		t.Errorf("addresses lost between metadata reads: %v", got.IPs)
	}
	if got.MacAddr == "" || got.RxBytes == nil {
		t.Errorf("metadata lost between metadata reads: %+v", got)
	}
	if got.RxMbps == first.Interfaces[0].RxMbps {
		t.Errorf("rates did not move on a rates-only tick: %v", got.RxMbps)
	}
}

// TestIfStatusRatesNeverWriteBackIntoTheHeldMetadata is the copy in Tick. Held
// rows are reused every tick, so a rate written into them would outlive the
// reading it came from — an interface would show its last known speed for ever.
func TestIfStatusRatesNeverWriteBackIntoTheHeldMetadata(t *testing.T) {
	r := &splitReader{byMenu: map[string]int{}}
	c := NewIfStatus(r, func(string, string, any) {}, "r1", 1000)
	c.Tick()

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.base) != 1 {
		t.Fatalf("held %d rows, want 1", len(c.base))
	}
	if c.base[0].RxMbps != 0 || c.base[0].TxMbps != 0 {
		t.Errorf("held metadata carries a rate: rx=%v tx=%v", c.base[0].RxMbps, c.base[0].TxMbps)
	}
}

// TestIfStatusSlowPollReadsEveryMenuEveryTick records the collapse case, and it
// is the branch the frozen golden takes. If this ever fails, the golden replay
// is no longer exercising the code it was recorded against.
func TestIfStatusSlowPollReadsEveryMenuEveryTick(t *testing.T) {
	r := &splitReader{byMenu: map[string]int{}}
	c := NewIfStatus(r, func(string, string, any) {}, "r1", 30000)
	if n := c.metaTicks(); n != 1 {
		t.Fatalf("a 30s poll must not split; metaTicks = %d", n)
	}
	for i := 0; i < 3; i++ {
		c.Tick()
	}
	for _, menu := range []string{
		"/interface/print", "/ip/address/print",
		"/interface/ethernet/print", "/interface/monitor-traffic",
	} {
		if got := r.byMenu[menu]; got != 3 {
			t.Errorf("%s read %d times over 3 ticks, want 3", menu, got)
		}
	}
}

// TestIfStatusSuspendArmsTheNextMetadataRead: a blurred page can sit for an hour,
// and resuming must not publish an hour-old interface list beside a live rate.
func TestIfStatusSuspendArmsTheNextMetadataRead(t *testing.T) {
	r := &splitReader{byMenu: map[string]int{}}
	c := NewIfStatus(r, func(string, string, any) {}, "r1", 1000)
	c.Tick()
	before := r.byMenu["/interface/print"]

	c.Suspend()
	c.Tick()

	if got := r.byMenu["/interface/print"]; got != before+1 {
		t.Errorf("first tick after Suspend read metadata %d times, want %d", got-before, 1)
	}
}

// ── THE FAST/SLOW RULE ACROSS COLLECTORS ────────────────────────────────────

type routeSplitReader struct{ byMenu map[string]int }

func (r *routeSplitReader) Connected() bool { return true }
func (r *routeSplitReader) Do(cmd routeros.Cmd) ([]routeros.Reply, error) {
	r.byMenu[cmd.Path]++
	switch cmd.Path {
	case "/ip/route/print", "/ipv6/route/print":
		return []routeros.Reply{{".id": "*1", "dst-address": "0.0.0.0/0", "gateway": "198.51.100.1", "active": "true"}}, nil
	case "/routing/bgp/session/print":
		return []routeros.Reply{{"name": "peer1", "established": "true"}}, nil
	}
	return nil, nil
}

// TestRoutingSplitsTheRouteTables: BGP session state is what flaps and stays on
// the poll; the route tables are large, rarely move, and go to the slow lane.
func TestRoutingSplitsTheRouteTables(t *testing.T) {
	r := &routeSplitReader{byMenu: map[string]int{}}
	c := NewRouting(r, func(string, string, any) {}, 10000)

	const ticks = 9
	for i := 0; i < ticks; i++ {
		c.Tick()
	}

	if got := r.byMenu["/routing/bgp/session/print"]; got != ticks {
		t.Errorf("BGP read %d times, want one per tick (%d)", got, ticks)
	}
	want := ticks / routeConfigEvery
	for _, menu := range []string{"/ip/route/print", "/ipv6/route/print"} {
		if got := r.byMenu[menu]; got != want {
			t.Errorf("%s read %d times over %d ticks, want %d", menu, got, ticks, want)
		}
	}
}

// TestRoutingFirstTickReadsTheRoutes pins tick 0 taking the slow lane too: a
// payload whose route list is empty until the third poll is a blank page, not a
// stale one.
func TestRoutingFirstTickReadsTheRoutes(t *testing.T) {
	r := &routeSplitReader{byMenu: map[string]int{}}
	c := NewRouting(r, func(string, string, any) {}, 10000)
	c.Tick()
	if got := r.byMenu["/ip/route/print"]; got != 1 {
		t.Errorf("first tick read the route table %d times, want 1", got)
	}
	if p := c.Last(); p == nil || len(p.Routes) == 0 {
		t.Errorf("first payload carries no routes: %+v", p)
	}
}

// TestRoutingWriteDoesNotWaitForTheSlowLane: RefreshNow is what the route write
// path calls, and it must re-read immediately rather than let an operator watch
// their own edit take half a minute to appear.
func TestRoutingWriteDoesNotWaitForTheSlowLane(t *testing.T) {
	r := &routeSplitReader{byMenu: map[string]int{}}
	c := NewRouting(r, func(string, string, any) {}, 10000)
	c.Tick()
	before := r.byMenu["/ip/route/print"]
	c.RefreshNow()
	if got := r.byMenu["/ip/route/print"]; got != before+1 {
		t.Errorf("RefreshNow read the route table %d extra times, want 1", got-before)
	}
}

// ── PHASE 4.1: THE DERIVATION, TESTED WITHOUT A COLLECTOR ───────────────────
//
// `BuildIfStatus` is the worked example for extracting a derivation as a pure
// function. The point of the extraction is exactly this: the error and drop
// figures the page shows are DELTAS, and until now the only way to test one was
// to drive a whole collector through two ticks against a fake router. Prior state
// as a PARAMETER makes it two calls and no I/O at all.

func ifRow(name string, rxErr, rxDrop string) routeros.Reply {
	return routeros.Reply{
		"name": name, "type": "ether", "running": "true", "disabled": "false",
		"rx-error": rxErr, "tx-error": "0",
		"rx-drop": rxDrop, "tx-drop": "0", "tx-queue-drop": "0",
	}
}

func TestBuildIfStatusDifferencesTwoReadings(t *testing.T) {
	t0 := time.Now()

	base, snap, delta := BuildIfStatus(nil, IfStatusInput{
		Ifaces: []routeros.Reply{ifRow("ether1", "5", "2")},
		Addrs:  []routeros.Reply{{"interface": "ether1", "address": "198.51.100.1/24"}},
		Now:    t0,
	})
	if len(base) != 1 {
		t.Fatalf("first reading built %d rows, want 1", len(base))
	}
	// THE FIRST READING HAS NO BASELINE, and must not invent one. A zero delta
	// here would read as "no errors in this window" on a link that has five.
	if len(delta) != 0 {
		t.Errorf("first reading produced a delta with nothing to subtract from: %v", delta)
	}
	if base[0].ErrorsDelta != nil {
		t.Errorf("first reading carries an error delta: %v", *base[0].ErrorsDelta)
	}

	base2, _, delta2 := BuildIfStatus(snap, IfStatusInput{
		Ifaces: []routeros.Reply{ifRow("ether1", "9", "2")},
		Addrs:  []routeros.Reply{{"interface": "ether1", "address": "198.51.100.1/24"}},
		Now:    t0.Add(30 * time.Second),
	})
	d, ok := delta2["ether1"]
	if !ok || d.errors == nil {
		t.Fatalf("second reading produced no error delta: %+v", delta2)
	}
	if *d.errors != 4 {
		t.Errorf("error delta = %v, want 4 (9 minus 5)", *d.errors)
	}
	if d.windowMs != 30000 {
		t.Errorf("delta window = %vms, want 30000 — the window is the gap between "+
			"the two readings, not the poll", d.windowMs)
	}
	if base2[0].DeltaWindowMs == nil || *base2[0].DeltaWindowMs != 30000 {
		t.Errorf("the window did not reach the payload: %+v", base2[0].DeltaWindowMs)
	}
}

// TestBuildIfStatusIsPure pins the property the extraction exists for: same
// inputs, same outputs, and the prior state handed in is not written through.
func TestBuildIfStatusIsPure(t *testing.T) {
	in := IfStatusInput{Ifaces: []routeros.Reply{ifRow("ether1", "5", "2")}, Now: time.Now()}
	prior := map[string]counterSnap{}

	a, snapA, _ := BuildIfStatus(prior, in)
	b, snapB, _ := BuildIfStatus(prior, in)

	if len(prior) != 0 {
		t.Errorf("the prior state passed in was mutated: %v", prior)
	}
	if len(a) != len(b) || a[0].Name != b[0].Name || len(snapA) != len(snapB) {
		t.Errorf("two identical calls disagreed: %+v vs %+v", a, b)
	}
}

// TestBuildIfStatusRefusesToBuildFromNothing: nil is "keep what you have", and
// the caller depends on it. Returning an empty slice instead would blank the
// Interfaces page on one failed read.
func TestBuildIfStatusRefusesToBuildFromNothing(t *testing.T) {
	base, snap, delta := BuildIfStatus(nil, IfStatusInput{Now: time.Now()})
	if base != nil || snap != nil || delta != nil {
		t.Errorf("no rows produced a payload: %v %v %v", base, snap, delta)
	}
}

// TestBuildFirewallRuleReadsPriorAndReturnsNext pins the discipline that makes
// the stateful derivations safe to extract: the function READS the prior
// counters and never writes them, returning the new baseline for the caller to
// store. A derivation that advanced the baseline itself would move it even when
// the caller discarded the result.
func TestBuildFirewallRuleReadsPriorAndReturnsNext(t *testing.T) {
	prev := map[string]fwCount{}
	row := routeros.Reply{".id": "*1", "chain": "input", "action": "accept", "packets": "100", "bytes": "9000"}

	first, count := BuildFirewallRule(prev, "filter", row)
	if first.DeltaPackets != 0 {
		t.Errorf("first reading invented a delta: %d", first.DeltaPackets)
	}
	if len(prev) != 0 {
		t.Errorf("the derivation wrote to the prior state it was handed: %v", prev)
	}
	if count.packets != 100 {
		t.Errorf("returned baseline = %d packets, want 100", count.packets)
	}

	prev[countKey("filter", "*1")] = count
	row["packets"] = "140"
	second, _ := BuildFirewallRule(prev, "filter", row)
	if second.DeltaPackets != 40 {
		t.Errorf("delta = %d, want 40 (140 minus 100)", second.DeltaPackets)
	}

	// A COUNTER THAT WENT BACKWARDS is a rule recreated on the same id, not
	// forty negative packets. The clamp is what stops the page showing one.
	row["packets"] = "5"
	third, _ := BuildFirewallRule(prev, "filter", row)
	if third.DeltaPackets != 0 {
		t.Errorf("a reset counter produced delta %d, want 0", third.DeltaPackets)
	}
}

// TestBuildLanOverviewIsAFunctionOfItsInputs: the largest derivation extracted,
// and it carries no state at all. Two identical calls must agree, and the lease
// list it is handed must not be modified.
func TestBuildLanOverviewIsAFunctionOfItsInputs(t *testing.T) {
	in := LanInput{
		Nets:     []routeros.Reply{{"address": "192.168.88.0/24", "gateway": "192.168.88.1"}},
		Addrs:    []routeros.Reply{{"interface": "ether1", "address": "198.51.100.2/24"}},
		LeaseIPs: []string{"192.168.88.10", "192.168.88.11"},
		WanIface: "ether1",
		Now:      time.Now(),
	}
	a := BuildLanOverview(in)
	b := BuildLanOverview(in)

	if len(in.LeaseIPs) != 2 {
		t.Errorf("the lease list handed in was modified: %v", in.LeaseIPs)
	}
	if a.WanIP != "198.51.100.2/24" {
		t.Errorf("WAN address = %q, want the address on the named interface", a.WanIP)
	}
	if len(a.Networks) != 1 || a.Networks[0].LeaseCount != 2 {
		t.Errorf("lease count did not land on the subnet: %+v", a.Networks)
	}
	if len(a.Networks) != len(b.Networks) || a.WanIP != b.WanIP {
		t.Errorf("two identical calls disagreed")
	}
}
