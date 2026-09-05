package alertpool

import (
	"fmt"
	"log"
	"time"

	"mikrodash/internal/collect"
	"mikrodash/internal/routeros"
)

// primeDeadline bounds how long PrimeStats will hold its caller up, AND how long
// the read it starts may stay outstanding.
//
// A read on an open socket comes back in tens of milliseconds; this is the
// allowance for a router that has stopped answering without its connection
// having dropped yet.
//
// ── THE SAME BOUND ON BOTH, DELIBERATELY ───────────────────────────────────
//
// This used to bound only the WAIT. The read itself carried no timeout, so
// `routeros.Client.Do` ran on `context.Background`, `reader.Do` held the
// router's `roslimit` slot for as long as that took, and the goroutine outlived
// the call that started it. Every Devices focus on a connected-but-unanswering
// router parked another one — and a router switch is three focus calls, so the
// slots went in threes.
//
// Stamping the deadline on the command makes the read end when the wait does,
// which is what "not cancelled" should always have meant. Nothing is waiting for
// a late result anyway: `syncAlertPool` drops the session as soon as the
// overview summary is `Known`, so a reading that lands after the frame lands in
// a session nobody reads.
const primeDeadline = 1500 * time.Millisecond

// primeReader is `reader` with a deadline stamped on every command.
//
// A wrapper rather than a field on `reader`, because the collectors' own reads
// must stay unbounded: they are polls with their own cadence, and a slow one
// blocks nothing but its own loop.
type primeReader struct {
	reader
	within time.Duration
}

func (r primeReader) Do(c routeros.Cmd) ([]routeros.Reply, error) {
	c.Timeout = r.within
	return r.reader.Do(c)
}

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
	cand := make([]*poolSession, 0, len(p.sessions))
	for _, s := range p.sessions {
		// A session that HAS a system collector is already answering; priming it
		// would take a second reading of the same gauges and overwrite nothing.
		if s.system == nil {
			cand = append(cand, s)
		}
	}
	p.mu.Unlock()

	// CLAIMED OUTSIDE `p.mu`, so the pool lock is never held while a session
	// lock is taken. The two are otherwise unordered here, and nesting them
	// would be the kind of thing that only shows up under load.
	//
	// The claim is what stops a focus from stacking reads on a router that is
	// not answering. `devicesFocus` is reached from `pageFocus` AND from
	// `selectRouter` via `rejoinPage`, so switching router is three calls; each
	// used to start its own goroutine and take its own `roslimit` slot on a
	// router that had already failed to answer the first one.
	todo := make([]*poolSession, 0, len(cand))
	for _, s := range cand {
		if s.startPriming() {
			todo = append(todo, s)
		}
	}
	if len(todo) == 0 {
		return
	}

	// CONCURRENTLY, because these are separate routers and the wait is entirely
	// network. `reader.Do` takes the per-router budget, so this cannot open more
	// channels on one router than anything else here would.
	//
	// ONE BUFFERED CHANNEL, counted as answers arrive, rather than a WaitGroup
	// and a second pass over `primedSystem()`. The second pass counted the
	// FIELD, and the field is never cleared — so a value left by an earlier
	// focus counted as this call's success and a router that had stopped
	// answering still logged "primed 3/3", pointing away from the hang the line
	// exists to find. Buffered to the full width so a goroutine that finishes
	// after the deadline can still send and exit rather than blocking for ever.
	start := time.Now()
	results := make(chan bool, len(todo))
	for _, s := range todo {
		go func(s *poolSession) {
			defer s.donePriming()
			results <- s.primeSystem(within)
		}(s)
	}

	deadline := time.NewTimer(within)
	defer deadline.Stop()
	ok, answered := 0, 0
wait:
	for answered < len(todo) {
		select {
		case good := <-results:
			answered++
			if good {
				ok++
			}
		case <-deadline.C:
			break wait
		}
	}

	// LOGGED, because this is the only place the cost is visible. It runs on a
	// page focus and not on the tick, so it is one line per cold open of the
	// Devices page — and when the first paint is blank again, the answer to
	// "did anything prime, and how long did it take" is the first thing needed.
	//
	// The two numbers are now about THIS call: `ok` counts readings this call
	// stored, and anything it did not hear back from inside the deadline is
	// named separately rather than being folded into the same failure bucket as
	// a session with no connection.
	msg := ""
	if late := len(todo) - answered; late > 0 {
		msg = fmt.Sprintf(", %d did not answer in time", late)
	}
	log.Printf("[alertpool] primed %d/%d collector-less session(s) in %s%s",
		ok, len(todo), time.Since(start).Round(time.Millisecond), msg)
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
func (s *poolSession) primeSystem(within time.Duration) bool {
	if s.closed() {
		return false
	}
	c := collect.NewSystem(primeReader{reader{s}, within},
		func(string, string, any) {}, s.eff.Poll["system"])
	// ONE COMMAND, which is the whole claim this makes. A fresh collector has a
	// zero `healthAt`, so its first Tick would ask `/system/health/print` before
	// the gauges — a second roslimit-gated command per router for `TempC`, which
	// nothing outside `internal/collect` reads.
	c.DeferHealth()
	c.Tick()
	p := c.Last()
	if p == nil {
		return false
	}
	s.mu.Lock()
	s.primed = p
	s.mu.Unlock()
	return true
}

// primedSystem is what the last prime read, or nil if none has succeeded.
func (s *poolSession) primedSystem() *collect.SystemPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.primed
}

// startPriming claims this session for one prime, and reports whether the claim
// was granted. donePriming releases it.
//
// The pair exists because the read it guards can outlast the call that started
// it: a router whose socket is up but which has stopped answering holds its
// reader for the whole deadline, and the Devices page can focus three times in
// that window. Without the claim each focus started another goroutine holding
// another `roslimit` slot on the one router least able to spare it.
func (s *poolSession) startPriming() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.priming {
		return false
	}
	s.priming = true
	return true
}

func (s *poolSession) donePriming() {
	s.mu.Lock()
	s.priming = false
	s.mu.Unlock()
}
