package collect

import (
	"mikrodash/internal/hub"
	"testing"
)

// Phase 5.1's rule, in both directions.
//
// The operator's requirement is that no page waits for data. Seeding is how the
// two GRAPHS meet that — they are the only cards whose value is a window rather
// than a reading, so `Last()` replay cannot serve them.
//
// The rule that makes seeding safe is that it never overwrites. A seed arriving
// after the collector has produced would REWIND the chart, and a rewind is worse
// than the blank it was meant to fix.

func TestTrafficSeedFillsAnEmptyRing(t *testing.T) {
	tr := NewTraffic(nil, hub.Relay{}, "ether1", 5)
	tr.Seed("ether1", []TrafficPoint{{TS: 1, RxMbps: 1}, {TS: 2, RxMbps: 2}})

	got := tr.History("ether1")
	if len(got.Points) != 2 || got.Points[1].RxMbps != 2 {
		t.Fatalf("seed did not reach the ring: %+v", got.Points)
	}
}

func TestTrafficSeedNeverOverwritesLiveData(t *testing.T) {
	tr := NewTraffic(nil, hub.Relay{}, "ether1", 5)
	tr.Seed("ether1", []TrafficPoint{{TS: 100, RxMbps: 9}})
	// A second seed, as a duplicated or late handover would deliver.
	tr.Seed("ether1", []TrafficPoint{{TS: 1, RxMbps: 1}, {TS: 2, RxMbps: 2}})

	got := tr.History("ether1")
	if len(got.Points) != 1 || got.Points[0].TS != 100 {
		t.Errorf("a second seed rewound the chart to %+v; a non-empty ring is live "+
			"data and always better than a seed", got.Points)
	}
}

// TestTrafficSeedIsBoundedByTheWindow. The pool's ring and the Session's may be
// sized differently — the pool builds Traffic with a fixed 5 — so a seed must be
// trimmed to THIS collector's window rather than trusted.
func TestTrafficSeedIsBoundedByTheWindow(t *testing.T) {
	tr := NewTraffic(nil, hub.Relay{}, "ether1", 1)
	var many []TrafficPoint
	for i := 0; i < 500; i++ {
		many = append(many, TrafficPoint{TS: int64(i)})
	}
	tr.Seed("ether1", many)

	got := tr.History("ether1")
	if len(got.Points) > 60 {
		t.Fatalf("a 1-minute window took %d points; maxPoints is minutes*60", len(got.Points))
	}
	// The NEWEST points, not the oldest: a chart seeded with the start of the
	// buffer would draw a window that ended before the viewer arrived.
	last := got.Points[len(got.Points)-1]
	if last.TS != 499 {
		t.Errorf("the trim kept the oldest points (last ts %d); it must keep the newest", last.TS)
	}
}

func TestPingSeedFillsAndNeverOverwrites(t *testing.T) {
	p := NewPing(nil, hub.Relay{}, 5000, "1.1.1.1")
	p.Seed([]PingPoint{{TS: 1}, {TS: 2}})
	if got := p.History(); len(got.History) != 2 {
		t.Fatalf("seed did not reach the history: %+v", got.History)
	}
	p.Seed([]PingPoint{{TS: 9}})
	if got := p.History(); len(got.History) != 2 || got.History[0].TS != 1 {
		t.Errorf("a second seed overwrote live history: %+v", got.History)
	}
}

// TestSeedIgnoresNothing — the handover calls these unconditionally, and a
// router the pool was not holding hands over an empty slice.
func TestSeedIgnoresNothing(t *testing.T) {
	tr := NewTraffic(nil, hub.Relay{}, "ether1", 5)
	tr.Seed("ether1", nil)
	tr.Seed("", []TrafficPoint{{TS: 1}})
	if got := tr.History("ether1"); len(got.Points) != 0 {
		t.Errorf("an empty seed produced %d points", len(got.Points))
	}
	p := NewPing(nil, hub.Relay{}, 5000, "1.1.1.1")
	p.Seed(nil)
	if got := p.History(); len(got.History) != 0 {
		t.Errorf("an empty ping seed produced %d points", len(got.History))
	}
}
