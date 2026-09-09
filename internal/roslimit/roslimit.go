// Package roslimit bounds how many API commands may be in flight against ONE
// router at a time.
//
// ── THE CONSTRAINT IS THE DEVICE, NOT THIS PROCESS ──────────────────────────
//
// CLAUDE.md states the bottleneck plainly: "more efficient means fewer router
// channels, not faster payload assembly". A hAP ac2 does not care what this
// server's memory looks like; it cares how many API channels are open on it.
//
// Nothing bounded that. Every collector owns its own timer goroutine -- 29
// poll loops across the three pools -- and each one calls `reader.Do` whenever
// its interval elapses. The client is safe for concurrent use (async mode tags
// every call), so nothing errors; the commands simply all go out at once. The
// staggered startup in `startCollectors` spreads the FIRST tick by 75 ms per
// burst group and says nothing about the steady state, where independent
// intervals drift into alignment on their own.
//
// ── AND WHY THE GATE IS KEYED BY ROUTER ─────────────────────────────────────
//
// Three separate pools reach the same devices: the viewing session
// (`internal/session`), the background pool for unwatched routers
// (`internal/routers`), and the alerting pool (`internal/alertpool`). A cap
// inside any one of them is not a cap on the router, because the other two keep
// their own count -- so a router being watched AND alerted AND polled for the
// Devices page would see three independent budgets.
//
// The gate is therefore process-wide and keyed by router id, which is the only
// key that matches what is actually scarce.
//
// ── WHAT THIS IS NOT ────────────────────────────────────────────────────────
//
// It is not the single-reader refactor. That remains a costed proposal in
// docs/architecture-next.md: 40-45 files, of which ~2,250 lines are
// source-scanning tests pinned to literal text that cannot be adapted, only
// re-authored. This is a dozen lines at the one function every collector read
// already passes through, and it delivers the documented goal. Revisit the
// refactor once this shows whether contention is real.
package roslimit

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultMax is deliberately generous rather than tuned.
//
// The point is to put a CEILING under a number that had none -- 29 poll loops
// could previously all be inside `Do` at once -- not to throttle normal work. A
// figure low enough to be felt would change collector timing, and timing is what
// 136 gates compare. Eight is above every steady-state burst measured here and
// far below the unbounded case.
const DefaultMax = 8

var (
	mu     sync.Mutex
	gates  = map[string]chan struct{}{}
	maxOne = -1 // resolved once, on first use

	// counts is commands issued per router since the last report.
	//
	// It lives under `mu` DELIBERATELY, rather than behind atomics or a second
	// mutex. `Acquire` already takes this lock to find the gate, so an increment
	// inside that critical section costs one map write and no extra
	// synchronisation. A separate lock would double the contention on the hot
	// path to count it, which is a strange trade for an instrument.
	counts = map[string]int64{}

	// menus is commands per RouterOS menu since the last report.
	//
	// The per-router total says how much this app costs a device; this says
	// WHERE it goes. Step 1.3 of Collectors-Rewrite.md needed it: the static
	// count of duplicated menus (26 per sweep) turned out not to predict the
	// per-minute cost at all, because the duplicated menus are read at very
	// different cadences. Optimising the static count optimises the wrong thing.
	menus = map[string]int64{}

	// streams is how many channels this process holds OPEN per router, right
	// now. A LEVEL, not a counter, and that is the whole distinction from the
	// two above.
	//
	// ── WHY THIS EXISTS, AND WHY IT IS NOT A GATE ───────────────────────────
	//
	// `Acquire` caps in-flight COMMANDS at eight, and a stream takes no slot:
	// `reader.Do` calls Acquire and `reader.Stream` does not. So until now this
	// process could hold any number of channels on a router and report nothing
	// about it -- against the bottleneck this project documents as concurrent
	// channels.
	//
	// Track B makes that a live question rather than a theoretical one, and B.4
	// enables collectors one at a time and MEASURES each. There was nothing to
	// measure: the number did not exist anywhere.
	//
	// IT IS AN INSTRUMENT, NOT A LIMIT, and deliberately so. B.0b searched to 24
	// concurrent channels on live hardware and found no ceiling, no starvation
	// and no CPU trend, so capping would enforce a bound nobody has observed. A
	// number that is reported and not enforced is the honest state of the
	// evidence.
	streams = map[string]int{}
)

// max reads the override once. An unparseable or non-positive value falls back
// rather than failing: this is a performance guard, and refusing to start over a
// malformed tuning knob would be a worse failure than ignoring it.
func max() int {
	if maxOne > 0 {
		return maxOne
	}
	maxOne = DefaultMax
	if v := os.Getenv("MIKRODASH_ROUTER_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxOne = n
		}
	}
	return maxOne
}

// Acquire blocks until this router has a free slot and returns the release.
//
// The returned function MUST be called, and callers use `defer` at the top of
// the wrapped call so an early return or a panic cannot leak a slot -- a leaked
// slot is permanent, and enough of them deadlock every collector on that router.
//
// An empty id is NOT gated. A router with no id is a test fixture or a session
// being torn down, and blocking those on a shared bucket would couple unrelated
// work.
func Acquire(routerID string) func() {
	if routerID == "" {
		return func() {}
	}
	mu.Lock()
	g, ok := gates[routerID]
	if !ok {
		g = make(chan struct{}, max())
		gates[routerID] = g
	}
	// Counted here, not at release: the question is how many commands this
	// process ASKS a router for, and one that blocks on a full gate has still
	// been asked for.
	counts[routerID]++
	mu.Unlock()

	g <- struct{}{}
	var once sync.Once
	return func() { once.Do(func() { <-g }) }
}

// Note records which menu a command was for. Called beside Acquire by each of
// the three readers, rather than folded into Acquire, so the concurrency gate
// keeps its signature and its single responsibility.
func Note(routerID, menu string) {
	if menu == "" {
		return
	}
	mu.Lock()
	menus[menu]++
	mu.Unlock()
}

// StreamOpened records that a channel is now open on this router, and returns
// the release.
//
// SHAPED LIKE `Acquire` ON PURPOSE, so a caller cannot tell them apart at the
// call site and cannot forget which one takes a release. It blocks on nothing:
// see the note on `streams` for why this counts rather than caps.
//
// An empty id is not counted, matching Acquire: a router with no id is a test
// fixture or a session being torn down.
func StreamOpened(routerID string) func() {
	if routerID == "" {
		return func() {}
	}
	mu.Lock()
	streams[routerID]++
	mu.Unlock()

	var once sync.Once
	return func() {
		// `once` OWNS IDEMPOTENCY, and there is deliberately no `> 0` guard
		// below. A collector torn down on both a blur and a disconnect releases
		// twice -- this app does that routinely -- and `once.Do` is what makes
		// the second call do nothing. A second guard inside it could never fire,
		// and a defence that cannot fire reads as though the invariant needs
		// two, which is how the next reader ends up preserving the wrong one.
		once.Do(func() {
			mu.Lock()
			streams[routerID]--
			if streams[routerID] <= 0 {
				// Removed rather than left at zero, so the report shows the
				// routers that HOLD channels rather than every router that ever
				// did. Same reason `Subscribe` deletes an empty menu.
				delete(streams, routerID)
			}
			mu.Unlock()
		})
	}
}

// OpenStreams reports how many channels this process holds on a router. For
// tests, diagnostics, and the stats line.
func OpenStreams(routerID string) int {
	mu.Lock()
	defer mu.Unlock()
	return streams[routerID]
}

// InFlight reports how many commands hold a slot for this router. For tests and
// diagnostics only.
func InFlight(routerID string) int {
	mu.Lock()
	defer mu.Unlock()
	return len(gates[routerID])
}

// StartStats logs how many commands each router was asked for, once per period.
//
// ── WHY THIS EXISTS, AND WHY IT IS OFF BY DEFAULT ───────────────────────────
//
// `Collectors-Rewrite.md` phase 1 rests on a measurement: the collectors issue
// 98 commands per full sweep against 72 distinct ones, so 26 are redundant. That
// figure is a static count of the code and an UPPER BOUND — it assumes every
// consumer is active at once, which the gating already prevents some of the
// time. This is the instrument that replaces the estimate with a number.
//
// Off unless MIKRODASH_CMD_STATS is set, because one line per period forever is
// noise on an install that is not being measured. Same idiom as
// MIKRODASH_ROUTER_CONCURRENCY above: an env knob read once, ignored if
// malformed, never a reason to refuse to start.
//
// ONE LINE FOR THE WHOLE FLEET, not one per router. A ten-router install would
// otherwise write 14,400 lines a day, and the total is the number phase 1 is
// judged on anyway.
func StartStats(every time.Duration) {
	if os.Getenv("MIKRODASH_CMD_STATS") == "" {
		return
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			mu.Lock()
			total := int64(0)
			parts := make([]string, 0, len(counts))
			for id, n := range counts {
				total += n
				parts = append(parts, fmt.Sprintf("%s=%d", short(id), n))
			}
			// The busiest menus, which is what says where the total goes.
			type mc struct {
				m string
				n int64
			}
			ms := make([]mc, 0, len(menus))
			for m, n := range menus {
				ms = append(ms, mc{m, n})
			}
			sort.Slice(ms, func(i, j int) bool { return ms[i].n > ms[j].n })
			// FOURTEEN, NOT EIGHT. Eight was enough while one collector was 76% of
			// the load; once the fast/slow split landed, the menus that had been
			// the top four fell out of the list entirely and the instrument could
			// no longer say whether they were at 2 a minute or 11. A tool for
			// finding where the total goes has to keep resolving it as the total
			// shrinks.
			tops := make([]string, 0, 14)
			for i, x := range ms {
				if i == 14 {
					break
				}
				tops = append(tops, fmt.Sprintf("%s=%d", x.m, x.n))
			}
			top := strings.Join(tops, " ")
			// ── THE OPEN CHANNELS, WHICH ARE A LEVEL AND ARE NOT CLEARED ─────
			//
			// `counts` and `menus` are reset each period because they measure
			// what happened during it. This measures what is TRUE NOW, so
			// clearing it would report zero for every router that opened its
			// channels before the tick.
			//
			// Read here rather than logged separately, so a reader sees commands
			// and channels for the same period on adjacent lines: Track B trades
			// one for the other, and the trade is unreadable if the two numbers
			// come from different minutes.
			held := make([]string, 0, len(streams))
			open := 0
			for id, n := range streams {
				open += n
				held = append(held, fmt.Sprintf("%s=%d", short(id), n))
			}
			clear(counts)
			clear(menus)
			mu.Unlock()

			// STREAMS ARE REPORTED EVEN IN A QUIET MINUTE, which commands are
			// not. Zero commands and twelve open channels is the state Track B
			// is aiming at, and the early-return below would have hidden exactly
			// that -- reporting nothing at the moment the app finally costs
			// nothing to poll.
			if open > 0 {
				sort.Strings(held)
				log.Printf("[roslimit] %d open stream(s) across %d router(s): %s",
					open, len(held), strings.Join(held, " "))
			}

			if total == 0 {
				continue // a quiet minute is not worth a line
			}
			sort.Strings(parts) // stable output, so two runs can be diffed
			log.Printf("[roslimit] %d commands in %s across %d router(s): %s",
				total, every, len(parts), strings.Join(parts, " "))
			if top != "" {
				log.Printf("[roslimit] busiest menus: %s", top)
			}
		}
	}()
}

// short trims a router id to something readable in a log line. The ids are
// UUIDs; the first segment is unique enough to tell a fleet apart.
func short(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return id
}

// Reset drops every gate. Tests only: it exists so one test's saturation cannot
// leak into the next, and calling it while commands are in flight would let them
// release into a channel nobody is holding.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	gates = map[string]chan struct{}{}
	counts = map[string]int64{}
	menus = map[string]int64{}
	// AND THE OPEN-CHANNEL LEVEL. Left out, one test's streams would be counted
	// against the next one's router -- and unlike the counters above this is a
	// LEVEL, so a leaked entry never decays and every later assertion in the
	// package would be measuring the leak.
	streams = map[string]int{}
	maxOne = -1
}
