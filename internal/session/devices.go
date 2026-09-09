package session

import (
	"sort"

	"mikrodash/internal/collect"
)

// What the Devices page reads about routers NOBODY IS WATCHING.
//
// ── THIS IS WHAT `internal/alertpool` WAS STILL FOR ─────────────────────────
//
// After 4.3 moved alerting and history onto held sessions, the alert pool built
// no collectors at all for the routers it still held: `buildCollectors`
// returned early unless the router had alerting or reporting on, and those
// routers are exactly the ones sessions now hold. `roslimit` confirmed it --
// 127 commands a minute across ONE router with three others connected.
//
// So the pool's whole remaining value was the SOCKET: a router it had dialled
// reported Connected, and the Devices page could say "up" the moment it
// rendered instead of showing a fleet of red Offline cards while the overview
// pool worked through them.
//
// `session.Reasons.Warm` is that socket, and these two methods are the reads
// that used to go to the pool. The payload fields stay in the shape the page
// already consumes, because a warm session has nil `System` and nil `IfStatus`
// for exactly the same reason the pool did.

// Snapshot is what the Devices page needs to know about a router it is not
// watching: is it reachable, and whatever the session already has.
//
// The same four fields `alertpool.Snapshot` carried, from the session that
// replaced it. `System` and `IfStatus` are nil for a WARM session, exactly as
// they were nil in the pool for a router with alerts and reporting off — the
// page's cold-open fix was always `Connected`, not the payloads.
type Snapshot struct {
	RouterID  string
	Connected bool
	System    *collect.SystemPayload
	IfStatus  *collect.IfStatusPayload
}

// Snapshots is every live session's view, for the Devices page.
func (m *Manager) Snapshots() []Snapshot {
	live := m.Live()
	out := make([]Snapshot, 0, len(live))
	for id, s := range live {
		snap := Snapshot{RouterID: id, Connected: s.Connected()}
		snap.System = s.systemReading()
		if snap.System == nil {
			// THE PRIME IS READ HERE OR NOWHERE. A warm session's system
			// collector is suspended and holds nothing, and prime.go's one-shot
			// reading is the only thing standing between that and a card with a
			// green badge over blank gauges.
			snap.System = s.primedSystem()
		}
		if ifs := s.IfStatus(); ifs != nil {
			snap.IfStatus = ifs.Last()
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RouterID < out[j].RouterID })
	return out
}

// Status is connected-or-not per router, for /healthz and the Devices page.
func (m *Manager) Status() map[string]bool {
	live := m.Live()
	out := make(map[string]bool, len(live))
	for id, s := range live {
		out[id] = s.Connected()
	}
	return out
}
