package collect

// A collector's subscription to one menu, with the poll-loop fallback.
//
// ── WHY THIS EXISTS ─────────────────────────────────────────────────────────
//
// Phase 3.2 moves collectors off their own timers: instead of owning a loop, a
// collector declares that it WANTS a menu at a cadence, and the router's one
// scheduler decides when to read it. `netwatch` and `talkers` were migrated by
// hand first, and doing two was enough to see that the interesting part is the
// same every time and the fiddly part is the same every time.
//
// The fiddly part is the fallback. BOTH BACKGROUND POOLS BUILD COLLECTORS WITH
// NO CACHE -- a router nobody is watching still needs netwatch for its alerts --
// so every migrated collector must keep working unchanged when `cache == nil`.
// Written out per collector that is four branches to get right in each of Start,
// Suspend, Resume, Reconnected and Stop, twenty times over. Written once it is
// this file, and a migration becomes a field plus five one-line delegations.
//
// ── WHAT IT DELIBERATELY DOES NOT DO ────────────────────────────────────────
//
// One menu. A collector reading several -- and most read several -- subscribes
// to the one whose cadence drives it, and reads the rest inside its own `apply`,
// through the cache, exactly as it did before. Modelling "derive when all of
// these are fresh" is a real question and it belongs to phase 4.2's views, not
// to a helper that exists to stop five lifecycle methods being copied.

import (
	"sync"
	"time"

	"mikrodash/internal/roscache"
	"mikrodash/internal/routeros"
)

// scheduled is embedded by a collector. The zero value is a collector that has
// not been given a cache, which is the polled path.
type scheduled struct {
	cache *roscache.Cache
	// loop is the fallback. Nil is allowed: a collector with neither a cache nor
	// a loop simply never reads, which is what a misconfigured one should do
	// rather than panicking on a nil dereference in a timer goroutine.
	loop *pollLoop

	menu    string
	fields  []string
	cadence func() time.Duration
	apply   func([]routeros.Reply, error)

	mu      sync.Mutex
	release func()
}

// useCache moves the collector onto the scheduler. Set once, before start.
func (s *scheduled) useCache(c *roscache.Cache) { s.cache = c }

// scheduling reports whether this collector is on the scheduler rather than its
// own loop, so a caller can keep a behaviour that only makes sense on one path --
// the immediate first read in Start, for instance.
func (s *scheduled) scheduling() bool { return s.cache != nil }

// begin declares demand, or starts the loop when there is no cache.
//
// IDEMPOTENT. Resume on a collector that is already running must not add a
// second subscription: the demand set counts subscribers, so a duplicate would
// keep the menu alive after the real one released it.
func (s *scheduled) begin() {
	if s.cache == nil {
		if s.loop != nil {
			s.loop.start()
		}
		return
	}
	s.mu.Lock()
	already := s.release != nil
	s.mu.Unlock()
	if already {
		return
	}

	cadence := time.Duration(0)
	if s.cadence != nil {
		cadence = s.cadence()
	}
	// NOT UNDER s.mu. Subscribe takes the cache's own lock, and the scheduler
	// takes that lock before calling `apply`, which takes the collector's. Doing
	// both here in the other order is how a deadlock gets built.
	rel := s.cache.Subscribe(s.menu, s.fields, cadence, s.apply)

	s.mu.Lock()
	if s.release != nil {
		// Two begins raced. Keep the first and give up the second rather than
		// leaking it, so the count still reaches zero when the collector stops.
		s.mu.Unlock()
		rel()
		return
	}
	s.release = rel
	s.mu.Unlock()
}

// end gives up the demand, or stops the loop.
//
// This is what Suspend now means. Not "stop my timer" but "stop wanting this
// menu" -- and when the last subscriber goes, the scheduler stops reading it.
// That is the single answer to "should this run" that phase 3 is for.
func (s *scheduled) end() {
	if s.cache == nil {
		if s.loop != nil {
			s.loop.stop()
		}
		return
	}
	s.mu.Lock()
	rel := s.release
	s.release = nil
	s.mu.Unlock()
	if rel != nil {
		rel()
	}
}
