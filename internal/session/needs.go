package session

// Which collectors a session runs, given why it is being kept alive.
//
// ── PHASE 4.3c: THE COST THAT MAKES THE MERGE A REGRESSION IF IGNORED ───────
//
// 4.3 replaces the background pools with sessions held for a non-viewer reason.
// Done naively that is a LOSS, and a large one. Measured on 2026-09-08:
//
//	the alert pool holding one unwatched router   119-120 commands/minute
//	a session's connect block                     15 collectors, against the
//	                                              pool's 7
//
// The nine extra are `bridges`, `dhcpLeases`, `dhcpNetworks`, `dns`, `firewall`,
// `logs`, `talkers`, `vlans` and `wan` -- and `wan` polls at two seconds,
// `talkers` at three, `bridges` at five. Roughly seventy-six extra commands a
// minute for a router NOBODY IS WATCHING, on an app whose documented bottleneck
// is concurrent channels on the MikroTik.
//
// So a session held only for alerting must run only what alerting needs. The
// collector set follows the HOLDERS, not the fact that a session exists.
//
// ── PURE, SO THE DECISION CAN BE TESTED WITHOUT A ROUTER ────────────────────
//
// Rows in, verdict out -- the same shape as the write guards and
// `internal/dormancy`, and for the same reason: every other part of this needs a
// live connection to exercise, and this is the part that decides what the app
// costs.

// Reasons is why a session is being kept alive. A viewer is one reason; the
// named holds are the others.
type Reasons struct {
	// Viewer is true while at least one browser has this router selected. It
	// wants everything, because it can navigate to any page.
	Viewer bool
	// Alerts is true for a router whose rules must be evaluated.
	Alerts bool
	// History is true for a router whose traffic and ping are being recorded.
	History bool
	// Devices is true while the Devices page is open, which reads a fixed set
	// per router.
	Devices bool
}

// historyFeeds are the two collectors continuous history is written from.
//
// `traffic` supplies per-interface throughput and `ping` the latency series;
// `internal/history` buckets both into the minute rows the tables take. Nothing
// else in the app writes a history row from a collector payload.
var historyFeeds = []string{"traffic", "ping"}

// devicesFeeds are what the Devices page reads per router. It matches what
// `internal/routers` builds for its overview sessions.
var devicesFeeds = []string{"system", "ifStatus", "traffic", "ping", "dhcpLeases"}

// Needs reports whether a collector should run, given why the session is alive.
//
// A VIEWER WANTS EVERYTHING. Not because every page is open, but because any of
// them can be, and the page gates decide the rest from there — that is the
// existing behaviour and this must not change it.
//
// Every other reason wants a NAMED set, because it consumes specific payloads
// and nothing else. That is the whole saving: an alerting router runs six
// collectors instead of fifteen.
func Needs(key string, why Reasons) bool {
	if why.Viewer {
		return true
	}
	if why.Alerts {
		for _, k := range AlertFeeds {
			if k == key {
				return true
			}
		}
	}
	if why.History {
		for _, k := range historyFeeds {
			if k == key {
				return true
			}
		}
	}
	if why.Devices {
		for _, k := range devicesFeeds {
			if k == key {
				return true
			}
		}
	}
	return false
}

// reasons reads the session's current holders. The caller holds s.mu.
func (s *Session) reasonsLocked() Reasons {
	return Reasons{
		Viewer:  s.refs > 0,
		Alerts:  s.holds["alerts"],
		History: s.holds["history"],
		Devices: s.holds["devices"],
	}
}

// applyReasons brings the running collector set into line with why this session
// is alive.
//
// ── IT DOES NOTHING WHILE A VIEWER IS PRESENT, AND THAT IS THE POINT ────────
//
// `Needs` says a viewer wants everything, which is a statement about what is
// ALLOWED to run, not what should be running now. Page gating decides the rest,
// and it is the reason an idle browser does not poll twenty-two collectors.
// Resuming everything here because a viewer exists would undo all of it.
//
// So the prescriptive case is the other one: with NO viewer, the session runs
// exactly the union of what its holders need, and nothing else. That is where
// the saving is — an alerting router runs six collectors instead of fifteen.
//
// IDEMPOTENT, and called from every transition rather than one: connect, a
// viewer arriving or leaving, a hold taken or dropped. The order those happen in
// is racy — a Retain releases its own viewer reference while the connect
// goroutine is still dialling — so this converges rather than being sequenced.
func (s *Session) applyReasons() {
	// NOTHING TO PRUNE BEFORE THE LINK IS UP. A hold taken while the session is
	// still dialling finds no collectors running, and the connect path calls this
	// itself once they are -- which is what makes the racy order between Retain
	// and the connect goroutine converge instead of needing to be sequenced.
	if s == nil || !s.Connected() {
		return
	}
	s.mu.Lock()
	why := s.reasonsLocked()
	s.mu.Unlock()

	if why.Viewer {
		return
	}
	targets := s.targets()
	for _, key := range targetKeys {
		t, ok := targets[key]
		if !ok {
			continue
		}
		if Needs(key, why) {
			// THROUGH THE FUNNEL, so the enabled check and the dormancy veto
			// still apply. A collector the operator turned off for this router
			// must not come back because alerting wants it.
			s.ResumeCollector(key)
			continue
		}
		t.suspend()
	}
}
