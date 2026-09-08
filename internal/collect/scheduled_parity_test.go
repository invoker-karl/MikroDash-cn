package collect

import (
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"mikrodash/internal/roscache"
	"mikrodash/internal/routeros"
)

// TestScheduledPathReadsEverythingThePolledPathDoes is the guard for a defect I
// shipped and had to find by hand.
//
// ── THE BUG ─────────────────────────────────────────────────────────────────
//
// A migrated collector subscribes to ONE menu and reads the rest inside `apply`.
// For `capsman` the block that loads the manager and CAP rows was left in `Tick`
// alone, on the reasoning -- written out in a comment, which made it look
// considered -- that a write marks `dirty` and `RefreshNow` ticks.
//
// RESUME BEGINS A SUBSCRIPTION WITHOUT TICKING. So a page blur and refocus left
// those rows empty for good, and the CAPsMAN page reported MODE Off on a router
// whose manager was enabled. Everything else on that page kept working, which is
// what made it look fine: 1 CAP, 4 radios, 28 clients, and one wrong word.
//
// The unit tests passed. The golden was unchanged. The race detector was clean.
// It was found by reading the page against the router.
//
// ── WHAT THIS ASSERTS ───────────────────────────────────────────────────────
//
// For each migrated collector: the set of menus it reads on the SCHEDULED path,
// after a Suspend and a Resume, must cover everything it reads on the POLLED
// path. Enough deliveries are driven to pass any config-lane cadence, because a
// menu read every twelfth time is still read.
//
// It is deliberately driven through `apply` rather than through a real scheduler.
// A scheduler test would have to wait out real cadences -- netwatch's is a flat
// sixty seconds -- and a test that sleeps for a minute is a test nobody runs.
func TestScheduledPathReadsEverythingThePolledPathDoes(t *testing.T) {
	cases := []struct {
		name string
		// subscribed is the menu the SCHEDULER fetches on the collector's behalf.
		// It is excluded from the comparison for the obvious reason: on the
		// scheduled path the collector never issues it itself, which is the whole
		// point. Naming it per case rather than inferring it keeps the exclusion
		// narrow -- a second menu going missing still fails.
		subscribed string
		// build returns the collector, its lifecycle, and the delivery that
		// stands in for the scheduler handing it rows.
		build func(Reader, *roscache.Cache) (start, suspend, resume func(), deliver func())
	}{
		{"capsman", "/interface/wifi/registration-table/print", func(r Reader, c *roscache.Cache) (func(), func(), func(), func()) {
			x := NewCapsman(r, func(string, string, any) {}, 10000)
			x.UseCache(c)
			return x.Start, x.Suspend, x.Resume, func() { x.apply(nil, nil) }
		}},
		{"bridges", "/interface/bridge/host/print", func(r Reader, c *roscache.Cache) (func(), func(), func(), func()) {
			x := NewBridges(r, func(string, string, any) {}, nil, 5000)
			x.UseCache(c)
			return x.Start, x.Suspend, x.Resume, func() { x.apply(nil, nil) }
		}},
		{"dns", "/ip/dns/print", func(r Reader, c *roscache.Cache) (func(), func(), func(), func()) {
			x := NewDNS(r, func(string, string, any) {}, 10000)
			x.UseCache(c)
			return x.Start, x.Suspend, x.Resume, func() { x.apply(nil, nil) }
		}},
		{"wan", "/interface/detect-internet/state/print", func(r Reader, c *roscache.Cache) (func(), func(), func(), func()) {
			x := NewWan(r, func(string, string, any) {}, nil, 10000)
			x.UseCache(c)
			return x.Start, x.Suspend, x.Resume, func() { x.apply(nil, nil) }
		}},
		{"rosusers", "/user/print", func(r Reader, c *roscache.Cache) (func(), func(), func(), func()) {
			x := NewRosUsers(r, func(string, string, any) {}, nil, 60000)
			x.UseCache(c)
			return x.Start, x.Suspend, x.Resume, func() { x.apply(nil, nil) }
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The POLLED baseline: no cache, so the collector ticks itself.
			polled := &menuRecorder{}
			pStart, _, _, _ := tc.build(polled, nil)
			pStart()

			// The SCHEDULED path, through a Suspend and a Resume.
			sched := &menuRecorder{}
			sStart, sSuspend, sResume, deliver := tc.build(sched, roscache.New(sched))
			sStart()
			sSuspend()
			sResume()
			// Enough to pass any config-lane cadence in the tree; the largest is
			// twelve.
			for i := 0; i < 30; i++ {
				deliver()
			}

			missing := []string{}
			for _, m := range polled.minus(sched) {
				if m != tc.subscribed {
					missing = append(missing, m)
				}
			}
			if len(missing) > 0 {
				t.Errorf("after Suspend and Resume, %s never reads %v, which its polled path "+
					"does. A lifecycle path that skips a read is invisible: the page keeps "+
					"working except for whatever those rows fed.", tc.name, missing)
			}
		})
	}
}

// menuRecorder is a Reader that answers nothing and remembers what it was asked.
// Empty replies are fine: this asks WHICH menus are read, not what comes back.
type menuRecorder struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (m *menuRecorder) Connected() bool { return true }

func (m *menuRecorder) Do(cmd routeros.Cmd) ([]routeros.Reply, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[string]bool{}
	}
	m.seen[cmd.Path] = true
	return nil, nil
}

func (m *menuRecorder) Stream(routeros.Cmd, func(routeros.Reply)) (func(), error) {
	return func() {}, nil
}

// reset forgets what has been seen, so a test can prime a collector and then
// observe only what it does next.
func (m *menuRecorder) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = map[string]bool{}
}

// minus returns the menus this recorder saw that the other did not.
func (m *menuRecorder) minus(other *menuRecorder) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	other.mu.Lock()
	defer other.mu.Unlock()

	var out []string
	for menu := range m.seen {
		if !other.seen[menu] {
			out = append(out, menu)
		}
	}
	sort.Strings(out)
	return out
}

var _ = strings.TrimSpace
var _ = time.Second

// TestResidualLoopsDoNotReadSubscribedMenus enforces the rule Mechanism A rests
// on, which `scheduled` cannot enforce itself.
//
// ── WHY THE RULE EXISTS ─────────────────────────────────────────────────────
//
// A residual collector reads on TWO schedules: the subscription drives what can
// be scheduled, the loop drives what cannot. That is safe only while they read
// different menus. Two clocks driving the SAME read is exactly the shape that
// produced a real bug in 3.2 -- the scheduler decided a menu was due and `Get`
// decided it was still fresh, so it refreshed at half its cadence, and every read
// looked ordinary in a log.
//
// `ifStatus` is the only residual collector today: the loop takes a rates
// MEASUREMENT from /interface/monitor-traffic, and the subscription plus its
// apply read /interface, /ip/address and /interface/ethernet. Disjoint.
func TestResidualLoopsDoNotReadSubscribedMenus(t *testing.T) {
	cases := []struct {
		name string
		// subscribed is the menu the scheduler fetches for this collector.
		subscribed string
		// loopSide drives the residual timer; subSide drives the subscription's
		// callback. Each is given its own recorder.
		loopSide func(Reader, *roscache.Cache)
		subSide  func(Reader, *roscache.Cache)
	}{
		{
			name: "ifStatus", subscribed: ifStatusIfCmd.Path,
			loopSide: func(r Reader, c *roscache.Cache) {
				x := NewIfStatus(r, func(string, string, any) {}, "r1", 1000)
				x.UseCache(c)
				// PRIMED FIRST, and the priming is part of the behaviour: with no
				// metadata there is nothing to attach a rate to, so Tick correctly
				// does nothing. The recorder is cleared after, so what remains is
				// the loop's own reads.
				x.applyMeta([]routeros.Reply{{"name": "ether1", "disabled": "false"}}, nil)
				if rec, ok := r.(*menuRecorder); ok {
					rec.reset()
				}
				x.Tick()
			},
			subSide: func(r Reader, c *roscache.Cache) {
				x := NewIfStatus(r, func(string, string, any) {}, "r1", 1000)
				x.UseCache(c)
				x.applyMeta([]routeros.Reply{{"name": "ether1"}}, nil)
			},
		},
		{
			name: "vpn", subscribed: vpnPppCmd.Path,
			loopSide: func(r Reader, c *roscache.Cache) {
				x := NewVPN(r, func(string, string, any) {}, 10000)
				x.UseCache(c)
				x.RefreshNow()
			},
			subSide: func(r Reader, c *roscache.Cache) {
				x := NewVPN(r, func(string, string, any) {}, 10000)
				x.UseCache(c)
				x.apply([]routeros.Reply{{"name": "peer1"}}, nil)
			},
		},
	}

	// ── THE TABLE MUST COVER EVERY RESIDUAL COLLECTOR ───────────────────────
	//
	// Finding them by source rather than trusting the table: a collector that
	// gains `residual: true` without an entry here would run two clocks with
	// nothing checking they stay apart, which is the failure this test exists
	// for. `vpn` was exactly that for one commit.
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[strings.ToLower(tc.name)] = true
	}
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading internal/collect: %v", err)
	}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(n)
		if err != nil || !strings.Contains(string(b), "residual: true") {
			continue
		}
		if !covered[strings.TrimSuffix(n, ".go")] {
			t.Errorf("%s sets residual:true and has no case here. It runs two clocks with "+
				"nothing checking they read different menus.", n)
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loop := &menuRecorder{}
			tc.loopSide(loop, roscache.New(loop))

			sub := &menuRecorder{}
			tc.subSide(sub, roscache.New(sub))
			// Plus the menu the scheduler fetches on its behalf.
			sub.mu.Lock()
			if sub.seen == nil {
				sub.seen = map[string]bool{}
			}
			sub.seen[tc.subscribed] = true
			sub.mu.Unlock()

			overlap := loop.minus(&menuRecorder{})
			var shared []string
			for _, m := range overlap {
				sub.mu.Lock()
				if sub.seen[m] {
					shared = append(shared, m)
				}
				sub.mu.Unlock()
			}
			sort.Strings(shared)

			if len(shared) > 0 {
				t.Errorf("%s's residual loop and its subscription both read %v. Two clocks on "+
					"one menu is the 3.2 bug: one decides it is due, the other decides it is "+
					"fresh, and it refreshes at half its cadence while every read looks "+
					"ordinary.", tc.name, shared)
			}
			if len(overlap) == 0 {
				t.Errorf("%s's residual loop read nothing — with a cache present it must still "+
					"drive the half that cannot be scheduled, which is the whole of mechanism A",
					tc.name)
			}
		})
	}
}
