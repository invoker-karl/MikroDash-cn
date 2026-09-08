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
// One menu AT A TIME. A collector reading several -- and most read several -- subscribes
// to the one whose cadence drives it, and reads the rest inside its own `apply`,
// through the cache, exactly as it did before. Modelling "derive when all of
// these are fresh" is a real question and it belongs to phase 4.2's views, not
// to a helper that exists to stop five lifecycle methods being copied.
//
// WHICH menu may change while running -- see `resubscribe` -- but there is still
// only ever one.

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

	// residual makes the loop the OTHER HALF rather than the fallback.
	//
	// Set it when part of a collector cannot be scheduled: `begin` then
	// subscribes AND starts the loop, and the loop drives only what the
	// subscription does not. `ifStatus` is the case it exists for -- three
	// metadata menus that schedule cleanly and a rates measurement that never
	// can.
	//
	// ── THE RULE THAT MAKES IT SAFE ─────────────────────────────────────────
	//
	// THE LOOP MUST NOT READ A MENU THE SUBSCRIPTION READS. Two clocks driving
	// different reads is fine; two clocks driving the SAME read is the shape that
	// produced a real bug in 3.2, where the scheduler and Get each judged
	// freshness and a menu refreshed at half its cadence.
	//
	// It is not enforceable from here -- this type cannot see what the loop's
	// body does -- so it is enforced from outside, by
	// TestResidualLoopsDoNotReadSubscribedMenus.
	residual bool

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
	// The other half, when there is one. Started before the subscription so a
	// collector is never briefly scheduled-but-not-ticking.
	if s.residual && s.loop != nil {
		s.loop.start()
	}
	s.mu.Lock()
	already := s.release != nil
	// Read under the lock, because `resubscribe` may move them. Everything
	// after this point works on THESE values and re-checks them before
	// committing, so a menu change that lands mid-subscribe loses rather than
	// half-applies.
	menu, fields, apply := s.menu, s.fields, s.apply
	cadence := s.cadence
	s.mu.Unlock()
	if already {
		return
	}

	d := time.Duration(0)
	if cadence != nil {
		d = cadence()
	}
	// NOT UNDER s.mu. Subscribe takes the cache's own lock, and the scheduler
	// takes that lock before calling `apply`, which takes the collector's. Doing
	// both here in the other order is how a deadlock gets built.
	rel := s.cache.Subscribe(menu, fields, d, apply)

	s.mu.Lock()
	if s.release != nil || s.menu != menu {
		// Two begins raced, or a resubscribe moved the menu underneath us. Keep
		// whatever won and give this one up rather than leaking it, so the count
		// still reaches zero when the collector stops.
		s.mu.Unlock()
		rel()
		return
	}
	s.release = rel
	s.mu.Unlock()
}

// resubscribe points the collector at a different menu, with the callback that
// knows how to read it.
//
// ── MECHANISM B: A MENU CHOSEN AT RUNTIME ───────────────────────────────────
//
// `begin`/`end` assume the collector knows its menu when it is constructed.
// Three do not. The firewall refreshes the counters of whichever TABLE the
// operator is looking at; `wifi` and `wireless` read whichever WIRELESS STACK
// the router turned out to have, which is not known until it answers. A
// subscription is per-menu, so those collectors need to be able to move one.
//
// ── WHY IT TAKES THE CALLBACK TOO, AND WHY THAT IS THE WHOLE POINT ──────────
//
// The delivered rows say nothing about which menu they came from. So a callback
// that resolves that itself -- reading the collector's "current table" field --
// can be handed the OLD menu's rows after the field has already moved, and merge
// nat counters into the filter table. The rows are keyed by RouterOS `.id`, and
// `*1` exists in every menu, so that merge SUCCEEDS and produces silently wrong
// numbers for one frame.
//
// Binding the callback to the menu at the moment of subscription removes the
// question. The caller passes a closure that already knows its table; nothing
// has to be resolved later, so nothing can be resolved late.
//
// ── ORDER ───────────────────────────────────────────────────────────────────
//
// Release first, then subscribe. The demand set counts subscribers per menu and
// stops reading a menu that has none, so releasing first is what makes the old
// menu actually go quiet.
//
// Releasing does NOT drain a delivery already under way. `Cache.deliver` copies
// the callbacks out and drops the lock before calling them, so an old-menu
// delivery can still land after the release returns. That is precisely why the
// callback is bound above rather than resolved later: the ordering cannot be
// relied on, so nothing is allowed to depend on it.
//
// A no-op when the menu has not changed, so a caller may pass its current
// selection unconditionally. Safe while suspended: it records the choice and
// `begin` picks it up.
func (s *scheduled) resubscribe(menu string, apply func([]routeros.Reply, error)) {
	s.mu.Lock()
	if s.menu == menu {
		s.mu.Unlock()
		return
	}
	s.menu, s.apply = menu, apply
	old := s.release
	s.release = nil
	fields, cadence := s.fields, s.cadence
	s.mu.Unlock()

	if old != nil {
		old()
	}
	// Nothing to move: either this collector is polled, or it is suspended and
	// `begin` will subscribe to the menu just recorded.
	if s.cache == nil || old == nil {
		return
	}

	d := time.Duration(0)
	if cadence != nil {
		d = cadence()
	}
	rel := s.cache.Subscribe(menu, fields, d, apply)

	s.mu.Lock()
	if s.release != nil || s.menu != menu {
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
	if s.residual && s.loop != nil {
		s.loop.stop()
	}
	s.mu.Lock()
	rel := s.release
	s.release = nil
	s.mu.Unlock()
	if rel != nil {
		rel()
	}
}
