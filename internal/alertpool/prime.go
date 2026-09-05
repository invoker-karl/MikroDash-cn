package alertpool

import (
	"log"
	"sync"
	"time"

	"mikrodash/internal/collect"
)

// primeDeadline bounds how long PrimeStats will hold its caller up.
//
// A read on an open socket comes back in tens of milliseconds; this is the
// allowance for a router that has stopped answering without its connection
// having dropped yet. A session that misses the deadline is not cancelled — its
// result still lands, and the next snapshot two seconds later carries it.
const primeDeadline = 1500 * time.Millisecond

// PrimeStats fills in a one-shot system reading for every session that has no
// system collector of its own.
//
// ── THE BLANK CARD UNDER THE GREEN BADGE ───────────────────────────────────
//
// A session with alerting off and reporting off builds NO collectors —
// `buildCollectors` returns before it makes any — which is exactly what the
// reporting toggle is for: a bare socket costs a connection and no command
// channels. The price is that `Snapshots` can then only answer `Connected`, so
// the Devices page draws a card that knows the router is up and nothing else:
// no CPU, no memory, no uptime, no model, for the two seconds the overview pool
// takes to dial its own connection.
//
// This closes that gap the cheapest way there is: ONE read, on a socket that is
// already open, at the moment somebody actually looks at the page. It costs
// nothing at all while nobody is looking, which is the property the toggle
// exists to protect.
//
// ONE READ IS LITERAL, and `primeSystem` has to work at it: a fresh
// `collect.System` would issue `/system/health/print` before the gauges, so the
// throwaway collector is told to defer that menu. See the note there.
//
// ── ONE TICK, NOT TWO ──────────────────────────────────────────────────────
//
// `System.Tick` does its static read from the SECOND tick on, so SERIAL and
// LICENCE LEVEL stay nil here. Fetching them would cost another command channel
// per router for two pills the overview pool fills in a couple of seconds
// anyway; the gauges are what the card looks empty without.
//
// ARCH IS NOT ONE OF THEM, though it reads like one: `architecture-name` comes
// back on the resource row itself, so the prime already has it.
func (p *Pool) PrimeStats() { p.primeStats(primeDeadline) }

// primeStats is PrimeStats with the deadline injected, so a test need not wait
// out a real one.
func (p *Pool) primeStats(within time.Duration) {
	p.mu.Lock()
	todo := make([]*poolSession, 0, len(p.sessions))
	for _, s := range p.sessions {
		// A session that HAS a system collector is already answering; priming it
		// would take a second reading of the same gauges and overwrite nothing.
		if s.system == nil {
			todo = append(todo, s)
		}
	}
	p.mu.Unlock()
	if len(todo) == 0 {
		return
	}

	// CONCURRENTLY, because these are separate routers and the wait is entirely
	// network. `reader.Do` takes the per-router budget, so this cannot open more
	// channels on one router than anything else here would.
	var wg sync.WaitGroup
	for _, s := range todo {
		wg.Add(1)
		go func(s *poolSession) {
			defer wg.Done()
			s.primeSystem()
		}(s)
	}
	start := time.Now()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(within):
	}

	// LOGGED, because this is the only place the cost is visible. It runs on a
	// page focus and not on the tick, so it is one line per cold open of the
	// Devices page — and when the first paint is blank again, the answer to
	// "did anything prime, and how long did it take" is the first thing needed.
	ok := 0
	for _, s := range todo {
		if s.primedSystem() != nil {
			ok++
		}
	}
	log.Printf("[alertpool] primed %d/%d collector-less session(s) in %s",
		ok, len(todo), time.Since(start).Round(time.Millisecond))
}

// primeSystem takes the reading, on the connection the session already holds.
//
// The collector is a THROWAWAY: it is never started, never emits — nothing
// consumes a status-only session's payloads, and handing this one to the
// evaluator would turn alerting back on for a router the operator switched it
// off for — and it is dropped as soon as its payload has been kept.
//
// A session with no connection needs no guard here: `reader` reports it as
// disconnected and `Tick` reads nothing, so the prime produces nothing rather
// than a zeroed reading. Reaching for `s.conn` directly instead is what would
// need one, and is the mistake this note exists to prevent.
func (s *poolSession) primeSystem() {
	if s.closed() {
		return
	}
	c := collect.NewSystem(reader{s}, func(string, string, any) {}, s.eff.Poll["system"])
	// ONE COMMAND, which is the whole claim this makes. A fresh collector has a
	// zero `healthAt`, so its first Tick would ask `/system/health/print` before
	// the gauges — a second roslimit-gated command per router for `TempC`, which
	// nothing outside `internal/collect` reads.
	c.DeferHealth()
	c.Tick()
	if p := c.Last(); p != nil {
		s.primedMu.Lock()
		s.primed = p
		s.primedMu.Unlock()
	}
}

// primedSystem is what the last prime read, or nil if none has succeeded.
func (s *poolSession) primedSystem() *collect.SystemPayload {
	s.primedMu.Lock()
	defer s.primedMu.Unlock()
	return s.primed
}
