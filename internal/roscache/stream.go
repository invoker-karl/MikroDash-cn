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
	// ── KIND ONE: THE ROWS ARE NOT READINGS OF ONE VALUE ────────────────────
	"/tool/ping": "every row is a distinct measurement counted into min/max/avg/loss; " +
		"a rolling entry keeps only the latest and would report 0% loss for ever",
	"/log/listen": "every row is a distinct event; a rolling entry drops lines",

	// ── KIND TWO WAS HERE AND IS NOW EMPTY ─────────────────────────────────
	//
	// It has been narrowed twice by measurement rather than by argument, and
	// both narrowings are worth keeping because each was a real obstacle:
	//
	//	FIRST it was CHURN. The entry could only accumulate, so a row that left
	//	the table never left the map -- closed connections, departed clients and
	//	expired leases piling up for the life of the session. B.6 found the round
	//	boundary (a repeated key, or a gap longer than the cadence) and that
	//	reason went. The connection table came off the list.
	//
	//	THEN it was EMPTINESS. A table with no rows sends nothing, and nothing is
	//	indistinguishable from a dead channel, so an emptied table held its last
	//	contents. The watchdog turned out to already run the distinguishing
	//	experiment: it reopens a quiet channel, and silence that survives a
	//	deliberate reopen is evidence of an empty table rather than a broken one.
	//	See the rule in `watch`.
	//
	// So no menu is refused for either reason now. The list above -- rows that
	// are not readings of one value -- is the one that remains, and it is
	// permanent: it is a fact about what a ping result and a log line ARE.
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

	mu sync.Mutex
	// rows is the last COMPLETE round: what `snapshot` serves.
	rows map[string]routeros.Reply
	// round is the round being received. See absorb for how its end is found.
	round map[string]routeros.Reply
	// published is false until the first round has completed, and while it is
	// false `snapshot` serves the ACCUMULATING round instead. Without it a page
	// would be blank for a whole interval after the channel opens, which is the
	// hang B.1 exists to remove rather than introduce.
	published bool
	// boundary is how long a quiet gap must be to end a round. Derived from the
	// subscription's cadence by the caller.
	boundary time.Duration
	lastRow  time.Time
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
	// rounds is how many complete rounds have been published.
	rounds int
	// quietSince is when the current run of silence began, reset by any row and
	// by an intentional reopen. See the empty-table rule in watch.
	quietSince time.Time
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
	keyOf func(routeros.Reply) string, boundary time.Duration) (func(), error) {
	return c.fillEvery(menu, cmd, keyOf, boundary, streamCheck, streamStale)
}

// fillEvery is FillFromStream with the watchdog's timings injected. Unexported:
// the intervals are a property of the mechanism, not a caller's choice.
func (c *Cache) fillEvery(menu string, cmd routeros.Cmd,
	keyOf func(routeros.Reply) string, boundary, check, stale time.Duration) (func(), error) {

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
	// ── SILENCE IS RELATIVE TO THE CADENCE, AND A FIXED BOUND IS A BUG ──────
	//
	// `streamStale` is ten seconds, which is right for a menu delivering every
	// second or two. It is WRONG for a slow one: `dhcpNetworks` runs at ten
	// MINUTES, so its channel is legitimately silent for ten minutes and a fixed
	// bound calls that death every ten seconds.
	//
	// MEASURED, not reasoned. With the fixed bound the DHCP page read "No DHCP
	// networks on this device" while the router held three, because the watchdog
	// restarted the channel constantly AND the empty-table rule -- which asks for
	// two staleness windows of silence -- concluded after twenty seconds that a
	// ten-minute menu was empty. The Go suite was green and the `=interval=`
	// probe passed; only the page was wrong.
	//
	// So a stream is silent when it has said nothing for longer than two of its
	// OWN intervals, and never less than the floor.
	if boundary > 0 && 2*boundary > stale {
		stale = 2 * boundary
	}
	f := &streamFill{cmd: cmd, keyOf: keyOf,
		rows: map[string]routeros.Reply{}, round: map[string]routeros.Reply{},
		boundary: boundary, check: check, stale: stale}
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

// absorb folds one pushed row into the round being received.
//
// ── FINDING THE END OF A ROUND, WHICH THE PROTOCOL DOES NOT MARK ────────────
//
// A `/print =interval=N` re-prints the WHOLE table every interval and sends no
// `!done` between rounds. Measured 2026-09-09: `/tool/netwatch/print` returned 9
// rows in 3s for 3 configured hosts, `/ip/dns/print` 4 rows for 1. Three
// re-prints, no separator.
//
// Without a boundary the entry can only ever accumulate, and a row that LEAVES
// the table never leaves the map -- closed connections, departed clients and
// expired leases pile up for the life of the session, on a page that looks
// populated. That is why every churning menu was refused.
//
// TWO SIGNALS, because neither is sufficient alone:
//
//	A KEY REPEATS   the round has restarted. Reliable precisely because the
//	                re-print is total: every row still present appears in every
//	                round, so a repeat is certain unless the entire membership
//	                turned over at once. This is the signal that works when the
//	                table is large enough that rounds arrive back to back with no
//	                gap between them.
//	A QUIET GAP     nothing for longer than the cadence. This is what ends the
//	                round for a small table, where the rows arrive in a burst and
//	                then silence, and a repeat would otherwise be a whole
//	                interval away.
//
// ── WHAT THIS STILL DOES NOT SOLVE, NAMED RATHER THAN GLOSSED ──────────────
//
// A table that becomes COMPLETELY EMPTY sends nothing at all, which is
// indistinguishable from a stream that has died -- and the watchdog, correctly,
// treats prolonged silence as death and reopens. So an emptied table holds its
// last contents rather than emptying.
//
// That is a far smaller error than the unbounded growth it replaces, and it is
// bounded by the next row rather than by the session. It is still an error, and
// it is why the registration tables stay refused: "no wireless clients" is an
// ordinary state, and ghost clients would be the visible result.
func (f *streamFill) absorb(r routeros.Reply) {
	k := f.keyOf(r)
	f.mu.Lock()
	defer f.mu.Unlock()

	// A GAP ENDS THE PREVIOUS ROUND, and this is checked before the repeat so a
	// small table's round closes on time rather than an interval late.
	if f.boundary > 0 && len(f.round) > 0 && !f.lastRow.IsZero() &&
		time.Since(f.lastRow) > f.boundary {
		f.finishRoundLocked()
	}
	f.lastRow = time.Now()
	// Any row ends the run of silence, which is what makes the empty-table rule
	// self-correcting rather than sticky.
	f.quietSince = time.Time{}

	if k == "" {
		// NOT DROPPED SILENTLY. A menu whose rows this cannot name is one a
		// rolling map cannot represent, and the count is what makes that
		// visible from outside instead of appearing as a page missing a row.
		f.unkeyed++
		return
	}
	if _, repeat := f.round[k]; repeat {
		f.finishRoundLocked()
	}
	if f.round == nil {
		f.round = map[string]routeros.Reply{}
	}
	f.round[k] = r
}

// finishRoundLocked publishes the round just received. Caller holds f.mu.
func (f *streamFill) finishRoundLocked() {
	if len(f.round) == 0 {
		return
	}
	f.rows = f.round
	f.round = map[string]routeros.Reply{}
	f.published = true
	f.rounds++
}

// hasRows reports whether this fill has received anything at all yet.
//
// ── A STREAM THAT HAS NOT WARMED UP IS A MISS, NOT AN EMPTY ANSWER ─────────
//
// `FillFromStream` returns as soon as the channel is OPEN, and the first row
// arrives some milliseconds later. The scheduler can deliver in that window, and
// `Get` answering "no rows" there is not a cheap wrong answer -- it is published
// to the collector, which builds an empty payload, and the next delivery is a
// whole cadence away.
//
// MEASURED: the DHCP page read "0 leases" and "No DHCP networks on this device"
// while the router held 45 and 3. Both menus run at TEN MINUTES, so one empty
// answer at startup persisted for ten minutes. On a fast menu the same race
// exists and self-corrects in a second, which is exactly why it would have been
// found late and blamed on something else.
//
// So an unwarmed fill falls through to the ordinary read path: the page is
// answered from a poll, and the stream takes over the moment it has rows.
// ── "AUTHORITATIVE", NOT "HAS ROWS", AND THE DIFFERENCE IS NOT PEDANTIC ────
//
// Written as `len(rows) > 0` this sends an entry that has legitimately gone
// EMPTY back to the read path -- so a genuinely empty table would be polled for
// ever AND hold a channel, which is worse than either alone. An entry that has
// completed a round is authoritative about its own emptiness.
//
// So the question is whether a round has ever completed, not whether there is
// anything in it.
func (f *streamFill) authoritative() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.published || len(f.round) > 0
}

// snapshot is the current value of every key, sorted. See the header on why the
// order is load-bearing rather than tidy.
func (f *streamFill) snapshot() []routeros.Reply {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := f.rows
	if !f.published {
		// The first round is still arriving. Serving it partially fills the page
		// a whole interval sooner than waiting, and the next round replaces it
		// wholesale.
		src = f.round
	}
	keys := make([]string, 0, len(src))
	for k := range src {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]routeros.Reply, 0, len(keys))
	for _, k := range keys {
		out = append(out, src[k])
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
			// A ROUND THAT HAS GONE QUIET IS OVER. `absorb` can only notice a
			// gap when the NEXT row arrives, which for a table read once a
			// minute is a minute late; this closes it on time. Same rule, the
			// other side of the silence.
			if f.boundary > 0 && len(f.round) > 0 && !f.lastRow.IsZero() &&
				time.Since(f.lastRow) > f.boundary {
				f.finishRoundLocked()
			}

			// ── AN EMPTY TABLE SENDS NOTHING, AND SO DOES A DEAD STREAM ─────
			//
			// RouterOS emits no rows at all for a `/print =interval=N` on an
			// empty table -- measured, not assumed: the B.4 probe held
			// `/ppp/active/print` open for three seconds on four routers and
			// received nothing, and that menu is empty on all of them.
			//
			// So silence is ambiguous, and holding the last contents was the
			// safe reading: an emptied table went on showing rows that had gone.
			// That is why the registration tables, the lease table and
			// `/ppp/active` stayed on the polled path after B.6.
			//
			// THE WATCHDOG RESOLVES IT, because it already does the experiment.
			// It reopens a channel that has gone quiet, and a REOPENED channel
			// on a router that is answering delivers at once if the table has
			// rows. Silence that survives a deliberate restart is therefore
			// evidence of an empty table rather than of a broken one.
			//
			// The rule: once restarted for silence, a further full staleness
			// window with nothing publishes an EMPTY round.
			//
			// IT CAN STILL BE WRONG, and the direction matters. A router that
			// has wedged in a way a reconnect does not clear would be reported
			// as having an empty table rather than a stale one. That is the
			// better error for these menus -- "no clients associated" invites a
			// look, while three clients that left an hour ago look entirely
			// plausible -- and it self-corrects on the first row that arrives.
			if f.restarts > 0 && f.published && len(f.rows) > 0 &&
				!f.quietSince.IsZero() && time.Since(f.quietSince) > 2*f.stale {
				f.rows = map[string]routeros.Reply{}
				f.round = map[string]routeros.Reply{}
				f.rounds++
				f.quietSince = time.Now()
			}

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
			// ── SET ONCE, NOT ON EVERY RESTART ──────────────────────────
			//
			// The silence run starts at the FIRST reopen and is not restarted by
			// later ones. Written as an unconditional assignment it could never
			// fire: the watchdog reopens every staleness window, so the run was
			// reset every window and never reached the two windows the
			// empty-table rule asks for. Caught by the test, which is the only
			// thing that could have caught it -- a rule that never fires looks
			// exactly like a rule that is not needed.
			if f.quietSince.IsZero() {
				f.quietSince = time.Now()
			}
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

// Unrollable reports why a menu may not back a rolling entry, if it may not.
//
// EXPORTED SO THE CALLER'S OWN TABLE CAN BE CHECKED AGAINST IT. Without this a
// line added to `session.streamableMenus` naming a refused menu is SAFE but
// MISLEADING: `fillIfStreaming` falls back to polling on any refusal, so the
// commit claims a delivery change, makes none, and nothing fails. The two lists
// disagreeing quietly is the exact shape `rooms.go` exists to stop.
func Unrollable(menu string) (string, bool) {
	why, no := unrollable[menu]
	return why, no
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
