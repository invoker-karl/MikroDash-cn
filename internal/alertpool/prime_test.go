package alertpool

import (
	"strings"
	"sync"
	"testing"
	"time"

	"mikrodash/internal/collect"
	"mikrodash/internal/routeros"
)

// primeConn answers the one menu the prime read asks for, and counts what it was
// asked. The COUNT is the point: "did not prime this session" is otherwise
// indistinguishable from "primed it and the result went nowhere".
type primeConn struct {
	mu   sync.Mutex
	up   bool
	cmds []string
}

func (c *primeConn) Do(cmd routeros.Cmd) ([]routeros.Reply, error) {
	c.mu.Lock()
	c.cmds = append(c.cmds, cmd.Path)
	c.mu.Unlock()
	if cmd.Path != "/system/resource/print" {
		// Health, routerboard and licence all answer emptily. A collector treats
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
	// NO `defer p.Close()`. Nothing was dialled, so there is no goroutine to
	// stop — and teardown reads `system != nil` as "this session has all six
	// alert collectors", which is true of every session the pool builds and not
	// of the one below.
	p := New((&primeDial{}).dial, time.Minute, nil, nil, nil)

	c := &primeConn{up: true}
	p.mu.Lock()
	p.sessions["x"] = &poolSession{
		r: Router{ID: "x"}, stop: make(chan struct{}), conn: c,
		// Never started, so it issues nothing of its own.
		system: collect.NewSystem(nil, nil, 0),
	}
	p.mu.Unlock()

	p.primeStats(time.Second)

	if n := c.reads(); n != 0 {
		t.Errorf("PrimeStats read the gauges %d time(s) on a session that already "+
			"collects them (saw %s); the guard on a nil collector is gone", n, c.saw())
	}
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

	p.primeStats(time.Second)

	if got := s.primedSystem(); got != nil {
		t.Errorf("primed %+v from a session with no connection", got)
	}
}
