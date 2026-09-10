package server

import (
	"time"

	"mikrodash/internal/collect"
	"mikrodash/internal/session"
)

// Phase 4.2b: demand replaces the page-to-collector switchboard.
//
// ── WHAT THIS REPLACES ──────────────────────────────────────────────────────
//
// A browser opening a page joins a room, and the server then looked that page up
// in a hand-written list and called the matching collector: 21 `ResumeCollector`
// calls and 19 blur cases in `ws.go`. The server was a switchboard that knew
// "page X means collector Y".
//
// `rooms.go` already declares every collector's rooms and the hub already knows
// who is in each room, so that mapping was derivable and the switchboard stated
// it a second time.
//
// ── STATING IT TWICE HAS GONE WRONG FIVE TIMES ─────────────────────────────
//
// The `routing` blur case carries the scar: the dashboard's Routes and BGP Peers
// cards read that collector, so a Routing-page blur stopped meaning "nobody is
// watching" -- and `blur-suspend-audit` caught the same consequence for
// `dhcpNetworks`, `bandwidth`, `vpn` and `firewall` before it. One bug, five
// collectors, because two places stated one fact.
//
// ── THE RULE, WHICH IS ONE SENTENCE ────────────────────────────────────────
//
//	a collector runs if anybody is in any room it declares
//
// plus the consumers that occupy no room and never will.

// wantsCollector answers whether one collector should be running for this
// session, and it is the whole of the new gate.
//
// THREE WAYS TO BE WANTED, and only the first is about a viewer:
//
//	somebody is in a room it feeds   the ordinary case
//	alerting needs it                the rules are not in a room and never will
//	                                 be, so occupancy always answers no for them
//	a non-viewer hold needs it       a session held for history or warm has no
//	                                 viewer at all
//
// ENABLEMENT IS NOT ASKED HERE. `ResumeCollector` already refuses a collector the
// operator disabled, and asking twice would put the same rule in two places --
// which is the defect this whole step exists to remove.
func (s *Server) wantsCollector(rs *session.Session, routerID, key string) bool {
	if rs == nil || routerID == "" {
		return false
	}
	if rs.NeededForAlerts(key) {
		return true
	}
	if rs.NeededForHolds(key) {
		return true
	}
	// ── DemandRooms, NOT RoomsOf ────────────────────────────────────────────
	//
	// The audience plus the rooms this collector must stay alive FOR. `ifStatus`
	// is the case: it emits to Interfaces, Topology and the Physical Ports card,
	// and four more pages borrow its rates without it ever sending them anything.
	// Asking `RoomsOf` here would suspend it for a viewer on Bridges and blank
	// every throughput column on the page.
	return s.roomsOccupied(routerID, collect.DemandRooms(key))
}

// applyDemand brings every collector's running state into line with who is
// actually listening.
//
// ── EVERY COLLECTOR, NOT THE ONES A LIST HAPPENED TO NAME ──────────────────
//
// The switchboard covered whichever pages somebody had written a case for. A
// collector missing from it was never gated at all -- it ran from connect to
// teardown, and nothing said so. Asking `session.TargetKeys()` means the question
// is asked of all of them, and a new collector is gated the moment it has rooms.
//
// IDEMPOTENT, so it is safe to call on any event that could change the answer: a
// page focus, a page blur, a socket leaving, a router switch. Resuming a running
// collector and suspending a stopped one are both no-ops.
func (s *Server) applyDemand(rs *session.Session, routerID string) {
	if rs == nil || routerID == "" {
		return
	}
	for _, key := range session.TargetKeys() {
		if s.wantsCollector(rs, routerID, key) {
			// THROUGH ResumeCollector, so the enablement check and the dormancy
			// latch still apply. A collector the operator turned off must not
			// come back because somebody opened a page.
			rs.ResumeCollector(key)
			continue
		}
		s.suspendAfterGrace(rs, routerID, key)
	}
}

// suspendAfterGrace stops a collector that nothing wants, a grace period later.
//
// ── THE GRACE IS INHERITED, AND IT IS NOT A REFINEMENT ─────────────────────
//
// `suspendIfNoRoomOccupied`, which this replaces, waited `s.graceFor()` before
// suspending and re-read the occupancy when the timer fired. Its reason applies
// unchanged: a page refresh empties every room this viewer was in and refills
// them a second later, and suspending on the empty moment stops the collector's
// stream and starts it again immediately -- churn on the one resource this
// project conserves, API channels, to save one second of polling.
//
// A page NAVIGATION is the same shape and is now far more common a trigger,
// because demand re-asks about every collector on every focus rather than about
// one page's collectors on a blur.
//
// THE QUESTION IS RE-ASKED WHEN THE TIMER FIRES, which is what makes this safe
// without tracking timers. Several may be in flight after rapid navigation; each
// re-asks, and all but the last find the collector wanted again and do nothing.
// A viewer who did not come back has their collector suspended late rather than
// never.
//
// It re-asks `wantsCollector` rather than re-reading the rooms, so a hold or an
// alert rule acquired during the grace is seen too -- the old timer read
// occupancy alone and would have suspended through one.
func (s *Server) suspendAfterGrace(rs *session.Session, routerID, key string) {
	time.AfterFunc(s.graceFor(), func() {
		if s.wantsCollector(rs, routerID, key) {
			return
		}
		// THE SEAM, and it is the same kind as `idleGrace` beside it: a field
		// only a test sets. `SuspendCollector` reaches every collector in the
		// session through a table of method values, so it cannot be called on a
		// Session a unit test can construct -- and the property worth asserting
		// here is not what Suspend does, it is that this timer re-asks and then
		// acts.
		if s.suspendOne != nil {
			s.suspendOne(rs, key)
			return
		}
		// NO "IS THE SESSION STILL LIVE?" CHECK, for the reason the helper this
		// replaces recorded: suspending a collector on a torn-down session is
		// inert, so the guard bought nothing and dereferenced the Manager on a
		// timer goroutine, where a nil is a dead process rather than a failed
		// request.
		rs.SuspendCollector(key)
	})
}
