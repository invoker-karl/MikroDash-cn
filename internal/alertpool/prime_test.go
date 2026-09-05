package alertpool

import (
	"strings"
	"sync"
	"testing"
	"time"

	"mikrodash/internal/collect"
	"mikrodash/internal/routeros"
)

// primeConn answers the one menu the prime read asks for, and RECORDS EVERY
// COMMAND it was asked for. The record is the point, twice over: "did not prime
// this session" is otherwise indistinguishable from "primed it and the result
// went nowhere", and a prime that quietly grew a second command would otherwise
// be invisible — which it was, until the list was asserted whole rather than
// filtered down to the read anyone expected.
type primeConn struct {
	mu       sync.Mutex
	up       bool
	cmds     []string
	timeouts []time.Duration
	// block, when non-nil, holds every Do until it is closed. It is how a
	// router that is up but has stopped answering is played.
	block chan struct{}
}

func (c *primeConn) Do(cmd routeros.Cmd) ([]routeros.Reply, error) {
	c.mu.Lock()
	c.cmds = append(c.cmds, cmd.Path)
	c.timeouts = append(c.timeouts, cmd.Timeout)
	block := c.block
	c.mu.Unlock()
	if block != nil {
		<-block
	}
	if cmd.Path != "/system/resource/print" {
		// A menu the prime does not ask for answers emptily. A collector treats
		// that as "nothing to report", which is what a board without the menu
		// does on a real router.
		return nil, nil
	}
	return []routeros.Reply{{
		"cpu-load": "7", "total-memory": "100", "free-memory": "40",
		"total-hdd-space": "100", "free-hdd-space": "90",
		"version": "7.24 (stable)", "board-name": "hAP ax^3", "uptime": "1d2h3m",
	}}, nil
}

func (c *primeConn) Stream(routeros.Cmd, func(routeros.Reply)) (func(), error) {
	return func() {}, nil
}
func (c *primeConn) Connected() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.up }
func (c *primeConn) Close() error    { c.mu.Lock(); c.up = false; c.mu.Unlock(); return nil }

func (c *primeConn) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, p := range c.cmds {
		if p == "/system/resource/print" {
			n++
		}
	}
	return n
}

func (c *primeConn) saw() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.cmds, ",")
}

// boundedBy reports whether every command carried a deadline, and which one.
func (c *primeConn) bounds() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration{}, c.timeouts...)
}

type primeDial struct {
	mu    sync.Mutex
	conns []*primeConn
}

func (d *primeDial) dial(routeros.Config) (Conn, error) {
	c := &primeConn{up: true}
	d.mu.Lock()
	d.conns = append(d.conns, c)
	d.mu.Unlock()
	return c, nil
}

// ── THE BLANK CARD UNDER THE GREEN BADGE ──────────────────────────────────
//
// A router with alerting and reporting both off runs NO collectors, so its
// snapshot could say "up" and nothing else and the Devices page drew a card with
// an empty CPU, memory and uptime for the two seconds the overview pool took to
// dial its own connection.
//
// Both halves are asserted, and the first is what makes the second mean
// anything: without it a `System` that was always populated would pass.
func TestPrimeStatsFillsAStatusOnlySessionsSnapshot(t *testing.T) {
	d := &primeDial{}
	p := New(d.dial, 10*time.Millisecond, nil, nil, nil)
	defer p.Close()

	p.Sync([]Router{{ID: "a", Label: "Alpha", Host: "198.51.100.1"}}, "", nil)
	waitFor(t, "the first observation", func() bool { return len(p.Snapshots()) == 1 })

	if got := p.Snapshots()[0]; got.System != nil {
		t.Fatalf("a status-only session reported System = %+v before priming; "+
			"it builds no collectors, so there is nothing that could have read it",
			got.System)
	}

	p.PrimeStats()

	got := p.Snapshots()[0]
	if got.System == nil {
		t.Fatal("PrimeStats left System nil: the Devices page still has a green " +
			"badge over blank gauges")
	}
	if got.System.CPULoad != 7 {
		t.Errorf("System.CPULoad = %d, want 7 from the primed read", got.System.CPULoad)
	}
	if got.System.UptimeRaw != "1d2h3m" {
		t.Errorf("System.UptimeRaw = %q, want the primed reading", got.System.UptimeRaw)
	}

	// ── AND IT COST EXACTLY ONE COMMAND ────────────────────────────────────
	//
	// The whole argument for priming on focus is that it is ONE read on a socket
	// that is already open; CLAUDE.md's measure of efficiency is router channels,
	// not CPU, so "one" is the claim that has to be pinned rather than assumed.
	// It was two: `collect.NewSystem` starts with a zero `healthAt`, so the very
	// first Tick asks `/system/health/print` before it asks for the gauges, and
	// the health row feeds one field — `TempC` — that nothing outside
	// `internal/collect` reads.
	d.mu.Lock()
	conns := append([]*primeConn{}, d.conns...)
	d.mu.Unlock()
	if len(conns) != 1 {
		t.Fatalf("%d connection(s) dialled, want 1", len(conns))
	}
	if saw := conns[0].saw(); saw != "/system/resource/print" {
		t.Errorf("the prime issued %q; it must be exactly one "+
			"/system/resource/print — every other menu here is a command "+
			"channel spent on a field no card renders", saw)
	}
}

// A session that HAS a system collector is already answering, and priming it
// would spend a command channel re-reading gauges the pool polls anyway.
//
// Built by hand rather than synced: a real alerting session polls in the
// background, so counting its reads across a window would race its own timer.
// Here nothing is running, so a read can only have come from PrimeStats.
func TestPrimeStatsLeavesASessionWithACollectorAlone(t *testing.T) {
	p := New((&primeDial{}).dial, time.Minute, nil, nil, nil)
	// TEARDOWN IS SAFE HERE, and it has to be built that way rather than
	// avoided. `stopCollectors` reads `system != nil` as "this session has all
	// six alert collectors" — true of every session the pool builds, and true of
	// this one only because all six are set below. The earlier version set
	// `system` alone and was defused solely by never calling `Close`, so adding
	// a cleanup, or a case that let `Sync` drop the session, would have panicked
	// on a nil `Stop` instead of failing on the property under test.
	t.Cleanup(p.Close)

	c := &primeConn{up: true}
	p.mu.Lock()
	p.sessions["x"] = &poolSession{
		r: Router{ID: "x"}, stop: make(chan struct{}), conn: c,
		// Never started, so they issue nothing of their own.
		system:   collect.NewSystem(nil, nil, 0),
		ping:     collect.NewPing(nil, nil, 0, ""),
		ifStatus: collect.NewIfStatus(nil, nil, "x", 0),
		vpn:      collect.NewVPN(nil, nil, 0),
		netwatch: collect.NewNetwatch(nil, nil, 0),
		routing:  collect.NewRouting(nil, nil, 0).BGPOnly(),
	}
	p.mu.Unlock()

	p.primeStats(time.Second, false)

	if n := c.reads(); n != 0 {
		t.Errorf("PrimeStats read the gauges %d time(s) on a session that already "+
			"collects them (saw %s); the guard on a nil collector is gone", n, c.saw())
	}
}

// ── A HISTORY-ONLY SESSION IS JUST AS BLANK, AND IS PRIMED TOO ─────────────
//
// Reporting ON, alerting OFF builds `ping` and `traffic` and no `system`, so its
// card has the same green badge over the same empty gauges. The filter is
// `system == nil`, which covers it — but that is invisible from the flags, and a
// later tightening to "alerting off AND reporting off" would silently un-fix it.
// This is the test that would fail if someone did.
func TestPrimeStatsAlsoFillsAHistoryOnlySession(t *testing.T) {
	d := &primeDial{}
	p := New(d.dial, 10*time.Millisecond, nil, nil, nil)
	defer p.Close()

	p.Sync([]Router{{
		ID: "h", Label: "History", Host: "198.51.100.4",
		ReportingEnabled: true, AlertsEnabled: false,
	}}, "", nil)
	waitFor(t, "the first observation", func() bool { return len(p.Snapshots()) == 1 })

	if got := p.Snapshots()[0]; got.System != nil {
		t.Fatalf("System = %+v before priming; a history-only session builds "+
			"ping and traffic, and neither reads the gauges", got.System)
	}

	p.PrimeStats()

	got := p.Snapshots()[0]
	if got.System == nil {
		t.Fatal("a history-only session was not primed: reporting on and " +
			"alerting off leaves no system collector, so its card has the same " +
			"green badge over the same blank gauges as a status-only one")
	}
	if got.System.CPULoad != 7 {
		t.Errorf("System.CPULoad = %d, want 7 from the primed read", got.System.CPULoad)
	}
}

// ── THE TICK FILLS A GAP ONCE, AND THEN LEAVES IT ALONE ────────────────────
//
// `PrimeStats` on focus covers the sessions that existed then; an interactive
// session idling out builds a connected, collector-less one WHILE the page is
// open, and nothing would prime it until the next focus. `PrimeUnread` is what
// the two-second tick calls, so it has to do both things: fill a session that
// has never answered, and cost nothing on one that has. The second half is the
// whole reason the prime is not a poll.
func TestPrimeUnreadFillsAGapOnceAndDoesNotPollIt(t *testing.T) {
	d := &primeDial{}
	p := New(d.dial, 10*time.Millisecond, nil, nil, nil)
	defer p.Close()

	p.Sync([]Router{{ID: "a", Label: "Alpha", Host: "198.51.100.1"}}, "", nil)
	waitFor(t, "the first observation", func() bool { return len(p.Snapshots()) == 1 })

	p.PrimeUnread()
	if got := p.Snapshots()[0]; got.System == nil {
		t.Fatal("PrimeUnread left an unread session unread — a router whose " +
			"interactive session idled out keeps its blank card until the next " +
			"focus")
	}

	d.mu.Lock()
	c := d.conns[0]
	d.mu.Unlock()
	if n := c.reads(); n != 1 {
		t.Fatalf("%d reads to fill the gap, want 1 (saw %s)", n, c.saw())
	}

	// Four more ticks. A session that has answered must not be asked again.
	for range 4 {
		p.PrimeUnread()
	}
	if n := c.reads(); n != 1 {
		t.Errorf("%d reads after four more ticks, want 1: re-reading a session "+
			"that already answered is exactly the poll the reporting toggle "+
			"exists to avoid (saw %s)", n, c.saw())
	}

	// And the focus path still REFRESHES, which is the difference between the
	// two entry points.
	p.PrimeStats()
	if n := c.reads(); n != 2 {
		t.Errorf("%d reads after a focus, want 2: PrimeStats takes a fresh "+
			"reading for the frame it is about to build", n)
	}
}

// ── THE READ ENDS WHEN THE WAIT DOES ───────────────────────────────────────
//
// The deadline used to bound only how long `primeStats` waited. The read itself
// carried no timeout, so `routeros.Client.Do` ran on `context.Background` and
// `reader.Do` held the router's `roslimit` slot until the router answered —
// which, for the router this whole path is about, is "possibly never". Stamping
// the command is what makes "not cancelled" stop meaning "outstanding for ever".
func TestThePrimeBoundsTheReadItStarts(t *testing.T) {
	d := &primeDial{}
	p := New(d.dial, 10*time.Millisecond, nil, nil, nil)
	defer p.Close()

	p.Sync([]Router{{ID: "a", Label: "Alpha", Host: "198.51.100.1"}}, "", nil)
	waitFor(t, "the first observation", func() bool { return len(p.Snapshots()) == 1 })

	p.primeStats(700*time.Millisecond, false)

	d.mu.Lock()
	conns := append([]*primeConn{}, d.conns...)
	d.mu.Unlock()
	if len(conns) != 1 {
		t.Fatalf("%d connection(s) dialled, want 1", len(conns))
	}
	for i, got := range conns[0].bounds() {
		if got != 700*time.Millisecond {
			t.Errorf("command %d carried Timeout %v, want the prime's own "+
				"deadline of 700ms; an unbounded read holds a roslimit slot "+
				"on the one router least able to spare it", i, got)
		}
	}
}

// ── ONE READ PER SESSION, NOT ONE PER FOCUS ────────────────────────────────
//
// `devicesFocus` is reached from `pageFocus` AND from `selectRouter` via
// `rejoinPage`, so switching router is three calls in quick succession. Against
// a router whose socket is up but which has stopped answering, each one used to
// start its own goroutine and take its own `roslimit` slot, on exactly the
// router that could least afford it.
func TestASecondPrimeDoesNotStackAReadOnTheSameSession(t *testing.T) {
	p := New((&primeDial{}).dial, time.Minute, nil, nil, nil)

	hold := make(chan struct{})
	c := &primeConn{up: true, block: hold}
	s := &poolSession{r: Router{ID: "x"}, stop: make(chan struct{}), conn: c}
	p.mu.Lock()
	p.sessions["x"] = s
	p.mu.Unlock()

	// The first focus: its read blocks, so it is still outstanding when the
	// second and third arrive. A short deadline so the call itself returns.
	first := make(chan struct{})
	go func() { defer close(first); p.primeStats(50*time.Millisecond, false) }()
	waitFor(t, "the first read to reach the router", func() bool {
		return len(c.bounds()) == 1
	})

	p.primeStats(50*time.Millisecond, false)
	p.primeStats(50*time.Millisecond, false)

	if n := len(c.bounds()); n != 1 {
		t.Errorf("%d reads outstanding (saw %s), want 1: a focus must not stack "+
			"a second read on a session that is already priming", n, c.saw())
	}

	close(hold)
	<-first
}

// A prime on a session whose connection has gone must not panic and must not
// invent a reading. The connect loop nils `conn` on the way down, and the
// Devices page can focus at exactly that moment.
func TestPrimeStatsToleratesASessionWithNoConnection(t *testing.T) {
	p := New((&primeDial{}).dial, time.Minute, nil, nil, nil)
	defer p.Close()

	p.mu.Lock()
	s := &poolSession{r: Router{ID: "x"}, stop: make(chan struct{})}
	p.sessions["x"] = s
	p.mu.Unlock()

	p.primeStats(time.Second, false)

	if got := s.primedSystem(); got != nil {
		t.Errorf("primed %+v from a session with no connection", got)
	}
}
