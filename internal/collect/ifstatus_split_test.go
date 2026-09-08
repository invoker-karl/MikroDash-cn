package collect

import (
	"strings"
	"testing"

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

	const ticks = 12
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
