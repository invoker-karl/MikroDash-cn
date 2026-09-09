package roscache

// B.1 of Collectors-Rewrite.md: a SECOND WAY OF FILLING AN ENTRY.
//
// ── THE ASSUMPTION THIS FILE EXISTS TO RETIRE ───────────────────────────────
//
// This cache was built for queries, and the shared-menu ledger recorded
// `/interface/monitor-traffic` as unroutable because "one side holds a stream,
// the other takes a bounded measurement; a by-menu cache would hand one the
// other's answer". True of the cache as it was. Not true of caching.
//
// The operator's challenge, 2026-09-09: "any stream here is simply the router
// controlling the rate of data arriving using =interval=N; a slow poll to the
// same endpoint would yield the same fields at a slower rate. So why not use
// cache?"
//
// It survives. The scheduler is only ONE filler:
//
//	poll     Scheduler tick -> Invalidate -> Get -> deliver(rows) -> onRows
//	stream   the router pushes ->            store -> deliver(rows) -> onRows
//	                                                  ^ the same seam
//
// `Cache.deliver` and `Subscribe`'s `onRows` are already the fan-out. A stream
// is a filler for an entry collectors already subscribe to, so a collector does
// not change at all to gain a stream path.
//
// ── THE BOUNDARY IS NOT QUERY-VERSUS-STREAM. IT IS WHAT A ROW MEANS ─────────
//
// A stream can back an entry when its rows are successive READINGS of a keyed
// value. It cannot when each row is a distinct element that matters:
//
//	/interface/monitor-traffic   a fresh reading per interface     ROLLABLE
//	any /print =interval=N       a re-print of the whole table     ROLLABLE
//	/tool/ping                   a distinct measurement; the collector counts
//	                             EVERY row into min/max/avg/loss   NOT ROLLABLE
//	/log/listen                  a distinct event                  NOT ROLLABLE
//
// Getting that wrong fails SILENTLY -- 0% loss for ever, dropped log lines, and
// nothing red anywhere. `unrollable` below is the gate, and it refuses rather
// than warns.
//
// ── NO SWEEP BOUNDARY, WHICH IS THE TRAP AVOIDED RATHER THAN SOLVED ────────
//
// `monitor-traffic` with a comma list pushes one row per interface per interval
// and sends no `!done` between rounds, so "wait for a complete sweep" has no
// signal to wait on. A ROLLING MAP has no boundary to detect: each row replaces
// its own key and the entry is always current.
//
// `snapshot` returns the values SORTED BY KEY. That is not tidiness: collectors
// fingerprint their payloads to suppress redundant emits, and an unstable order
// would make every payload look changed and defeat the dirty check.

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"mikrodash/internal/routeros"
)

// Streamer is the half of a reader that can hold a channel open. The cache's
// `Reader` is Do-only, because until now it had no use for the other half.
type Streamer interface {
	Stream(routeros.Cmd, func(routeros.Reply)) (func(), error)
}

// unrollable are the menus a rolling map would silently corrupt. Keyed by menu
// path, valued by the reason, so a refusal can say why.
//
// A DENYLIST AND NOT A JUDGEMENT AT THE CALL SITE. The failure is invisible --
// a ping collector fed a rolling entry reports 0% loss for ever and every test
// still passes -- so the decision belongs somewhere a person has to edit
// deliberately, next to the reason.
var unrollable = map[string]string{
	"/tool/ping": "every row is a distinct measurement counted into min/max/avg/loss; " +
		"a rolling entry keeps only the latest and would report 0% loss for ever",
	"/log/listen": "every row is a distinct event; a rolling entry drops lines",
}

// streamStale is how long an open channel may deliver nothing before the
// watchdog restarts it.
//
// ── LIFTED FROM `traffic`, WHOSE PROBLEM THIS NOW IS FOR EVERYONE ──────────
//
// A polled entry fails LOUDLY: the read errors and the error is cached and
// surfaced. A pushed entry can go SILENT -- the router stops sending, or the
// channel wedges -- and without this the cache would serve its last value for
// ever with nothing noticing. `internal/collect/traffic.go` already carried
// exactly this recovery for its one stream (5s tick, 10s staleness); here it
// covers every stream instead of one.
const (
	streamStale = 10 * time.Second
	streamCheck = 5 * time.Second
)

// streamFill keeps one menu's entry current from an open channel.
type streamFill struct {
	cmd   routeros.Cmd
	keyOf func(routeros.Reply) string

	mu      sync.Mutex
	rows    map[string]routeros.Reply
	lastRow time.Time
	// unkeyed counts rows the key function could not name. A menu that produces
	// any is one a rolling map cannot represent, and the count is the only way
	// to find that out from outside.
	unkeyed int
	stop    func()
	closed  bool
	// check and stale are the watchdog's tick and its silence bound. Fields
	// rather than the constants directly, so a test can drive the restart in
	// milliseconds instead of waiting out ten real seconds -- which is the
	// difference between this recovery being tested and being hoped for.
	check time.Duration
	stale time.Duration
	// restarts is how many times the watchdog has reopened this channel. Read by
	// a test, and worth having: a channel restarting steadily is a router
	// problem that would otherwise look like a slow page.
	restarts int
}

// FillFromStream opens a channel and keeps `menu`'s entry current from it, so
// `Get` on that menu answers from pushed rows instead of a read.
//
// `keyOf` names a row, and the caller MUST supply it. There is no default: an
// implicit key is how every row lands in one bucket and the entry silently
// becomes "the last row the router sent".
//
// The returned stop closes the channel and drops the fill. It is idempotent.
func (c *Cache) FillFromStream(menu string, cmd routeros.Cmd,
	keyOf func(routeros.Reply) string) (func(), error) {
	return c.fillEvery(menu, cmd, keyOf, streamCheck, streamStale)
}

// fillEvery is FillFromStream with the watchdog's timings injected. Unexported:
// the intervals are a property of the mechanism, not a caller's choice.
func (c *Cache) fillEvery(menu string, cmd routeros.Cmd,
	keyOf func(routeros.Reply) string, check, stale time.Duration) (func(), error) {

	if why, no := unrollable[menu]; no {
		return nil, fmt.Errorf("roscache: %s cannot be stream-filled: %s", menu, why)
	}
	if keyOf == nil {
		return nil, fmt.Errorf("roscache: %s needs a key function; a rolling map with no "+
			"key holds one row", menu)
	}
	s, ok := c.ros.(Streamer)
	if !ok {
		return nil, fmt.Errorf("roscache: this reader cannot stream")
	}

	c.mu.Lock()
	if c.fills == nil {
		c.fills = map[string]*streamFill{}
	}
	if _, dup := c.fills[menu]; dup {
		c.mu.Unlock()
		return nil, fmt.Errorf("roscache: %s is already stream-filled", menu)
	}
	f := &streamFill{cmd: cmd, keyOf: keyOf, rows: map[string]routeros.Reply{},
		check: check, stale: stale}
	c.fills[menu] = f
	c.mu.Unlock()

	if err := f.open(s); err != nil {
		c.mu.Lock()
		delete(c.fills, menu)
		c.mu.Unlock()
		return nil, err
	}

	done := make(chan struct{})
	go f.watch(s, done)

	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			f.close()
			c.mu.Lock()
			delete(c.fills, menu)
			c.mu.Unlock()
		})
	}, nil
}

// open starts the channel. The caller holds no lock.
func (f *streamFill) open(s Streamer) error {
	stop, err := s.Stream(f.cmd, f.absorb)
	if err != nil {
		return err
	}
	f.mu.Lock()
	if f.closed { // stopped while the stream was opening
		f.mu.Unlock()
		stop()
		return nil
	}
	f.stop, f.lastRow = stop, time.Now()
	f.mu.Unlock()
	return nil
}

// absorb folds one pushed row into the rolling map.
func (f *streamFill) absorb(r routeros.Reply) {
	k := f.keyOf(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastRow = time.Now()
	if k == "" {
		// NOT DROPPED SILENTLY. A menu whose rows this cannot name is one a
		// rolling map cannot represent, and the count is what makes that
		// visible from outside instead of appearing as a page missing a row.
		f.unkeyed++
		return
	}
	f.rows[k] = r
}

// snapshot is the current value of every key, sorted. See the header on why the
// order is load-bearing rather than tidy.
func (f *streamFill) snapshot() []routeros.Reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.rows))
	for k := range f.rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]routeros.Reply, 0, len(keys))
	for _, k := range keys {
		out = append(out, f.rows[k])
	}
	return out
}

func (f *streamFill) close() {
	f.mu.Lock()
	stop := f.stop
	f.stop, f.closed = nil, true
	f.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// watch is the silent-death recovery. See streamStale.
func (f *streamFill) watch(s Streamer, done <-chan struct{}) {
	t := time.NewTicker(f.check)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			f.mu.Lock()
			quiet := time.Since(f.lastRow)
			shut := f.closed
			stop := f.stop
			f.mu.Unlock()
			if shut || quiet < f.stale {
				continue
			}
			// STOPPED AND REOPENED, not closed: `close` sets `closed` and this
			// fill must survive its own restart.
			if stop != nil {
				stop()
			}
			f.mu.Lock()
			f.stop = nil
			f.restarts++
			// The rolling map is KEPT across a restart. Its rows are the last
			// readings the router gave and they are what a page renders while
			// the channel comes back; dropping them would blank every card for
			// the length of a reconnect, which is the opposite of the point.
			f.lastRow = time.Now()
			f.mu.Unlock()
			_ = f.open(s)
		}
	}
}

// StreamWhen installs the decision: for a menu about to be subscribed, may it be
// kept current by a channel instead of a read?
//
// ── ONE TABLE, NOT TWENTY-TWO COLLECTOR EDITS ───────────────────────────────
//
// The obvious plumbing was to give every collector a "should I stream" function
// at construction. That is twenty-two edits to say one thing, and twenty-two
// places for it to drift -- the defect `rooms.go` exists to record.
//
// The decision is keyed by MENU, which is what this package already speaks, and
// the caller resolves the rest: it knows which collector owns a menu and what
// `eff.Stream` says about it. So this is set ONCE per session and `scheduled`
// asks rather than being told.
//
// Nil means "poll everything", which is the state of every session until the
// caller says otherwise.
func (c *Cache) StreamWhen(fn func(menu string) bool) {
	c.mu.Lock()
	c.streamWhen = fn
	c.mu.Unlock()
}

// StreamsMenu is the decision for one menu. Exported because `scheduled` in
// internal/collect is the caller.
func (c *Cache) StreamsMenu(menu string) bool {
	c.mu.Lock()
	fn := c.streamWhen
	c.mu.Unlock()
	return fn != nil && fn(menu)
}

// fillFor returns the stream backing a menu, or nil.
func (c *Cache) fillFor(menu string) *streamFill {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fills[menu]
}

// StreamedMenus is which menus are currently stream-filled. For a test, and for
// anything that needs to know a menu is push-backed rather than polled.
func (c *Cache) StreamedMenus() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.fills))
	for m := range c.fills {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
