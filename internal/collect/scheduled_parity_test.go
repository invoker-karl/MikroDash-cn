package collect

import (
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
