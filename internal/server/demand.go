package server

import (
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
	return s.roomsOccupied(routerID, collect.RoomsOf(key))
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
		rs.SuspendCollector(key)
	}
}
