package session

// B.3: which menus may be delivered by a channel rather than a read.
//
// ── THE SWITCH IS WIRED HERE AND IT IS OFF ─────────────────────────────────
//
// `collection.Resolve` has computed `Resolved.Stream[key]` since the port, the
// UI has had the per-router Stream/Poll toggle since the port, and the value has
// reached the browser since the port. Nothing read it. Three of twenty-five
// collectors streamed, and an operator setting a router to Poll changed nothing
// because everything already polled.
//
// This is the seam that makes the setting mean something. It is deliberately not
// enough on its own.
//
// ── WHY IT IS A TABLE AND NOT A FLAG ON EACH COLLECTOR ─────────────────────
//
// Twenty-two collectors subscribe to a menu. Giving each a "should I stream"
// function is twenty-two edits to say one thing and twenty-two places for it to
// drift, which is the defect `internal/collect/rooms.go` was written to record.
// One table, keyed by the menu `roscache` already speaks, valued by the
// collector that owns it so `eff.Stream` can be asked about the right key.
//
// ── AND IT IS EMPTY, WHICH IS THE ACTIVATION HAZARD ────────────────────────
//
// `collection.defaultMode` is "stream". So the moment `eff.Stream` is consulted
// without a gate, every router that never expressed a preference switches ALL
// its dual-capable collectors to channels in one step -- a fleet-wide delivery
// change nobody asked for, on the strength of a default. Routers stored
// `mode: "poll"` would be honoured correctly; the ones nobody has an opinion
// about are the ones that would move.
//
// B.4 adds ONE LINE HERE PER COLLECTOR, in its own commit, measured against
// `roslimit`'s open-channel level on an unwatched router. That is what makes the
// change reversible one collector at a time instead of one release at a time.
//
// A menu absent from this table polls, which is what every collector does today.
var streamableMenus = map[string]string{
	// menu -> the registry key of the collector that owns it.
	//
	// Each line is one B.4 commit: enabled, then measured on an unwatched router
	// against `roslimit`'s open-channel level. `TestStreamableMenusAreRealAndOwned`
	// fails on an entry naming a menu nothing subscribes to or a collector the
	// registry does not have, so a line added carelessly does not silently do
	// nothing.

	// ── FIRST, AND CHOSEN FOR BEING BORING ──────────────────────────────────
	//
	// Real `.id` rows, a table of three, one page, a 30s cadence. It proves the
	// path end to end and saves almost nothing, which is the right trade for the
	// first one: the biggest prize is `system` at 111 commands a minute across
	// this fleet, and that is a SINGLE ROW WITH NO `.id` -- it needs a key of its
	// own, so it is not the collector to learn the mechanism on.
	//
	// Probed 2026-09-09: streams, 9 rows in 3s, first row 6ms.
	"/tool/netwatch/print": "netwatch",
}

// streamsMenu answers roscache's question: may this menu be pushed?
//
// TWO CONDITIONS, AND BOTH ARE REQUIRED. The table is this project's judgement
// that the menu is safe to stream at all; `eff.Stream` is the OPERATOR's per
// router decision. Either one saying no means poll, so enabling a collector here
// cannot override a router the operator has pinned to Poll.
func (s *Session) streamsMenu(menu string) bool {
	key, ok := streamableMenus[menu]
	if !ok {
		return false
	}
	return s.eff.Stream[key]
}
