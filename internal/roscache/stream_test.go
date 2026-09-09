package roscache

import (
	"errors"
	"sync"
	"testing"
	"time"

	"mikrodash/internal/routeros"
)

// pusher is a Reader that can also stream, and lets a test push rows by hand.
type pusher struct {
	mu     sync.Mutex
	onRow  func(routeros.Reply)
	opens  int
	stops  int
	refuse error
	reads  int
}

func (p *pusher) Do(routeros.Cmd) ([]routeros.Reply, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	return []routeros.Reply{{"from": "a read"}}, nil
}

func (p *pusher) Stream(_ routeros.Cmd, onRow func(routeros.Reply)) (func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refuse != nil {
		return nil, p.refuse
	}
	p.opens++
	p.onRow = onRow
	return func() {
		p.mu.Lock()
		p.stops++
		p.mu.Unlock()
	}, nil
}

func (p *pusher) push(rows ...routeros.Reply) {
	p.mu.Lock()
	fn := p.onRow
	p.mu.Unlock()
	for _, r := range rows {
		fn(r)
	}
}

func (p *pusher) counts() (opens, stops, reads int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opens, p.stops, p.reads
}

func byName(r routeros.Reply) string { return r["name"] }

func fill(t *testing.T, c *Cache, menu string) func() {
	t.Helper()
	stop, err := c.FillFromStream(menu, routeros.Cmd{Path: menu}, byName, time.Hour)
	if err != nil {
		t.Fatalf("FillFromStream(%s): %v", menu, err)
	}
	return stop
}

// TestAStreamedMenuAnswersGetWithoutReading is the whole premise of B.1: the
// scheduler is unchanged, and `Get` answers from pushed rows.
func TestAStreamedMenuAnswersGetWithoutReading(t *testing.T) {
	p := &pusher{}
	c := New(p)
	defer fill(t, c, "/interface/monitor-traffic")()

	p.push(routeros.Reply{"name": "ether1", "rx-bits-per-second": "100"})

	rows, err := c.Get("/interface/monitor-traffic", nil, time.Second)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rows) != 1 || rows[0]["rx-bits-per-second"] != "100" {
		t.Fatalf("Get returned %v, want the pushed row", rows)
	}
	if _, _, reads := p.counts(); reads != 0 {
		t.Errorf("%d read(s) issued for a streamed menu — the whole point is that "+
			"the router is already sending it", reads)
	}
}

// TestAKeyRepeatEndsTheRound.
//
// ── THIS TEST USED TO ASSERT A ROLLING MAP, AND B.6 IS WHY IT CHANGED ───────
//
// It was `TestARowReplacesItsOwnKeyAndOnlyThat`: three pushes of two names left
// two rows, with the second `ether1` replacing the first. That was right for a
// map that only ever accumulates, and it is wrong now.
//
// A `/print =interval=N` re-prints the WHOLE table each round with no separator,
// so a repeated key means the round has RESTARTED. The second `ether1` therefore
// belongs to the next round, and serving it would publish a table missing
// `ether2` — a page that flickers a row out and back on every interval.
//
// So the completed round is served whole, and the new one accumulates unseen.
func TestAKeyRepeatEndsTheRound(t *testing.T) {
	p := &pusher{}
	c := New(p)
	defer fill(t, c, "/interface/monitor-traffic")()

	p.push(
		routeros.Reply{"name": "ether1", "rx-bits-per-second": "100"},
		routeros.Reply{"name": "ether2", "rx-bits-per-second": "200"},
		routeros.Reply{"name": "ether1", "rx-bits-per-second": "999"}, // round 2 begins
	)

	rows, _ := c.Get("/interface/monitor-traffic", nil, time.Second)
	if len(rows) != 2 {
		t.Fatalf("%d rows, want the 2 of the completed round", len(rows))
	}
	if rows[0]["rx-bits-per-second"] != "100" || rows[1]["rx-bits-per-second"] != "200" {
		t.Errorf("served %v; a partial round was published, so the page would lose a "+
			"row and get it back on every interval", rows)
	}

	// Round two completes: now the newer reading is the one served.
	p.push(
		routeros.Reply{"name": "ether2", "rx-bits-per-second": "888"},
		routeros.Reply{"name": "ether1"}, // round 3 begins, publishing round 2
	)
	rows, _ = c.Get("/interface/monitor-traffic", nil, time.Second)
	if len(rows) != 2 || rows[0]["rx-bits-per-second"] != "999" {
		t.Errorf("after the second round completed, served %v — want the newer readings", rows)
	}
}

// TestARowThatLEAVESTheTableIsForgotten is the whole point of B.6, and the
// property whose ABSENCE kept nine menus refused.
//
// Without a round boundary the entry can only accumulate: a closed connection, a
// departed client or an expired lease stays for the life of the session, on a
// page that looks populated and is wrong.
func TestARowThatLeavesTheTableIsForgotten(t *testing.T) {
	p := &pusher{}
	c := New(p)
	defer fill(t, c, "/interface/monitor-traffic")()

	// Round one: three rows.
	p.push(routeros.Reply{"name": "a"}, routeros.Reply{"name": "b"}, routeros.Reply{"name": "c"})
	// Round two: `b` has gone. The repeat of `a` closes round one.
	p.push(routeros.Reply{"name": "a"}, routeros.Reply{"name": "c"})
	// Round three begins, publishing round two.
	p.push(routeros.Reply{"name": "a"})

	rows, _ := c.Get("/interface/monitor-traffic", nil, time.Second)
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, r["name"])
	}
	if len(names) != 2 || names[0] != "a" || names[1] != "c" {
		t.Errorf("served %v, want [a c] — the row that left the table was not "+
			"forgotten, which is the unbounded growth that kept the connection "+
			"table, the registration tables and the lease table on the polled path", names)
	}
}

// TestAQuietGapEndsTheRound. A repeat is a whole interval away for a small
// table, so silence is the other signal: rows arrive in a burst and stop.
func TestAQuietGapEndsTheRound(t *testing.T) {
	p := &pusher{}
	c := New(p)
	stop, err := c.fillEvery("/interface/monitor-traffic", routeros.Cmd{}, byName,
		20*time.Millisecond, 5*time.Millisecond, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	p.push(routeros.Reply{"name": "a"}, routeros.Reply{"name": "b"})
	// No repeat is coming; the watchdog tick must close the round on the gap.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f := c.fillFor("/interface/monitor-traffic")
		f.mu.Lock()
		done := f.rounds > 0
		f.mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	f := c.fillFor("/interface/monitor-traffic")
	f.mu.Lock()
	rounds := f.rounds
	f.mu.Unlock()
	if rounds == 0 {
		t.Error("a round that went quiet was never closed. For a table read once a " +
			"minute the next repeat is a minute away, so silence has to end it.")
	}
	if rows, _ := c.Get("/interface/monitor-traffic", nil, time.Second); len(rows) != 2 {
		t.Errorf("%d rows after the quiet round closed, want 2", len(rows))
	}
}

// TestTheSnapshotOrderIsStable. The collectors fingerprint their payloads to
// suppress redundant emits, so an unstable order makes every payload look
// changed — a page that redraws constantly and a dirty check that means nothing.
func TestTheSnapshotOrderIsStable(t *testing.T) {
	p := &pusher{}
	c := New(p)
	defer fill(t, c, "/interface/monitor-traffic")()

	// Pushed in an order that is not sorted, from a map that has no order.
	p.push(
		routeros.Reply{"name": "sfp1"},
		routeros.Reply{"name": "ether1"},
		routeros.Reply{"name": "bridge"},
	)

	want := []string{"bridge", "ether1", "sfp1"}
	// TWENTY TIMES, because Go's map iteration is randomised PER RANGE: one
	// agreeing pair proves nothing.
	for i := 0; i < 20; i++ {
		rows, _ := c.Get("/interface/monitor-traffic", nil, time.Second)
		for j, w := range want {
			if rows[j]["name"] != w {
				t.Fatalf("pass %d: rows[%d] = %q, want %q — the order is not stable",
					i, j, rows[j]["name"], w)
			}
		}
	}
}

// TestARowsAreEventsMenuIsRefused is the gate the design's safety rests on.
//
// Feeding `/tool/ping` a rolling map keeps only the latest measurement, so the
// collector's loss statistic — built by counting EVERY row — would read 0% for
// ever. Nothing errors, nothing logs, and every test in the tree still passes.
// That is why this refuses rather than warns.
func TestARowsAreEventsMenuIsRefused(t *testing.T) {
	c := New(&pusher{})
	for _, menu := range []string{"/tool/ping", "/log/listen"} {
		stop, err := c.FillFromStream(menu, routeros.Cmd{Path: menu}, byName, time.Hour)
		if err == nil {
			stop()
			t.Errorf("%s was accepted for stream-filling. Its rows are distinct "+
				"elements, not readings of one value, so a rolling map loses them "+
				"SILENTLY — which is the one failure this gate exists for.", menu)
		}
	}
}

// TestAFillNeedsAKeyFunction. An implicit key is how every row lands in one
// bucket and the entry quietly becomes "the last row the router sent".
func TestAFillNeedsAKeyFunction(t *testing.T) {
	c := New(&pusher{})
	if _, err := c.FillFromStream("/interface/print", routeros.Cmd{}, nil, time.Hour); err == nil {
		t.Error("a fill with no key function was accepted")
	}
}

// TestAnUnkeyableRowIsCountedNotDropped. A menu whose rows this cannot name is
// one a rolling map cannot represent; the count is how that is discoverable
// rather than appearing later as a page missing a row.
func TestAnUnkeyableRowIsCountedNotDropped(t *testing.T) {
	p := &pusher{}
	c := New(p)
	defer fill(t, c, "/interface/monitor-traffic")()

	p.push(routeros.Reply{"no-name-here": "x"}, routeros.Reply{"name": "ether1"})

	f := c.fillFor("/interface/monitor-traffic")
	f.mu.Lock()
	n := f.unkeyed
	f.mu.Unlock()
	if n != 1 {
		t.Errorf("unkeyed = %d, want 1 — a row this cannot name was dropped without trace", n)
	}
	if rows, _ := c.Get("/interface/monitor-traffic", nil, time.Second); len(rows) != 1 {
		t.Errorf("%d rows; the unkeyed row should not be in the map either", len(rows))
	}
}

// TestStoppingAFillReleasesTheChannelAndFallsBackToReading. A menu nobody
// streams any more must go back to being polled, not keep answering from a
// frozen map.
func TestStoppingAFillReleasesTheChannelAndFallsBackToReading(t *testing.T) {
	p := &pusher{}
	c := New(p)
	stop := fill(t, c, "/interface/monitor-traffic")
	p.push(routeros.Reply{"name": "ether1"})

	stop()
	stop() // idempotent, like every other release in this package

	if opens, stops, _ := p.counts(); opens != 1 || stops != 1 {
		t.Errorf("opens=%d stops=%d, want 1 and 1 — a double stop closed the channel twice",
			opens, stops)
	}
	if got := c.StreamedMenus(); len(got) != 0 {
		t.Errorf("still streaming %v after the stop", got)
	}
	if _, err := c.Get("/interface/monitor-traffic", nil, time.Second); err != nil {
		t.Fatalf("Get after the stop: %v", err)
	}
	if _, _, reads := p.counts(); reads != 1 {
		t.Errorf("%d read(s) after the fill was stopped, want 1 — the menu did not go "+
			"back to being polled and would answer from a frozen map for ever", reads)
	}
}

// TestARefusedStreamIsNotLeftRegistered. Otherwise the menu answers Get from an
// empty map for ever, and a page shows nothing while the cache reports success.
func TestARefusedStreamIsNotLeftRegistered(t *testing.T) {
	p := &pusher{refuse: errors.New("no")}
	c := New(p)
	if _, err := c.FillFromStream("/x", routeros.Cmd{}, byName, time.Hour); err == nil {
		t.Fatal("a refused stream reported success")
	}
	if got := c.StreamedMenus(); len(got) != 0 {
		t.Errorf("a refused stream left %v registered; Get would answer from an empty map", got)
	}
}

// TestTheSameMenuIsNotFilledTwice. Two fills on one menu means two channels on
// the router for one answer, and the second silently wins every Get.
func TestTheSameMenuIsNotFilledTwice(t *testing.T) {
	c := New(&pusher{})
	stop := fill(t, c, "/interface/monitor-traffic")
	defer stop()
	if _, err := c.FillFromStream("/interface/monitor-traffic", routeros.Cmd{}, byName, time.Hour); err == nil {
		t.Error("a second fill of the same menu was accepted — two channels, one answer")
	}
}

// ── THE WATCHDOG ────────────────────────────────────────────────────────────
//
// A polled entry fails LOUDLY: the read errors, and the error is cached and
// surfaced. A pushed entry can go SILENT — the router stops sending, or the
// channel wedges — and the cache would serve its last value for ever with
// nothing noticing. `traffic` already carried this recovery for its one stream;
// here it covers every stream, which is the point of moving it.
func TestASilentChannelIsRestarted(t *testing.T) {
	p := &pusher{}
	c := New(p)
	stop, err := c.fillEvery("/interface/monitor-traffic", routeros.Cmd{}, byName, time.Hour,
		5*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	p.push(routeros.Reply{"name": "ether1", "rx-bits-per-second": "100"})

	// Say nothing for well past the staleness bound.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if opens, _, _ := p.counts(); opens > 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	opens, stops, _ := p.counts()
	if opens < 2 {
		t.Fatalf("the channel went silent and was never reopened (opens=%d). A stream "+
			"that stops delivering would serve its last value for ever.", opens)
	}
	if stops < 1 {
		t.Errorf("the old channel was not closed before reopening (stops=%d) — the "+
			"router would accumulate a channel per restart", stops)
	}

	// THE ROWS SURVIVE THE RESTART, and that is deliberate. They are the last
	// readings the router gave and they are what a page renders while the
	// channel comes back; dropping them would blank every card for the length
	// of a reconnect, which is the opposite of what this is for.
	rows, _ := c.Get("/interface/monitor-traffic", nil, time.Second)
	if len(rows) != 1 || rows[0]["rx-bits-per-second"] != "100" {
		t.Errorf("the rolling map was cleared by the restart: %v", rows)
	}
}

// TestAHealthyChannelIsNotRestarted — the other direction, or the test above
// passes against a watchdog that simply restarts everything on a timer.
func TestAHealthyChannelIsNotRestarted(t *testing.T) {
	p := &pusher{}
	c := New(p)
	stop, err := c.fillEvery("/interface/monitor-traffic", routeros.Cmd{}, byName, time.Hour,
		5*time.Millisecond, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// A row every 20ms for 300ms: never silent for the 200ms bound.
	for i := 0; i < 15; i++ {
		p.push(routeros.Reply{"name": "ether1"})
		time.Sleep(20 * time.Millisecond)
	}
	if opens, _, _ := p.counts(); opens != 1 {
		t.Errorf("a channel delivering steadily was restarted %d time(s); the watchdog "+
			"is firing on a clock rather than on silence", opens-1)
	}
}

// TestAStreamedMenuStillFiresOnDeliver.
//
// ── A TRAP THIS DESIGN AVOIDS BY BEING ADDITIVE, PINNED SO IT STAYS AVOIDED ─
//
// `OnDeliver` is the dormancy supervisor's heartbeat: since phase 3.3 it rides
// the scheduler's deliveries instead of a ticker, and it is what decides a
// collector has stopped changing and may sleep. A stream filler that BYPASSED
// the scheduler — pushed rows straight to subscribers — would leave every
// streamed collector unjudged, so nothing would ever go dormant again. Nothing
// would fail; the router would just quietly never be left alone.
//
// This design does not bypass it: `Get` short-circuits for a streamed menu and
// the scheduler is otherwise UNCHANGED, so Invalidate/Get/deliver still runs at
// the subscriber's cadence and `deliver` still fires the hook. That is a
// consequence of the shape rather than a feature anyone wrote, which is exactly
// the kind of thing that is true until someone "optimises" the scheduler to skip
// streamed menus.
func TestAStreamedMenuStillFiresOnDeliver(t *testing.T) {
	p := &pusher{}
	c := New(p)
	defer fill(t, c, "/interface/monitor-traffic")()

	var mu sync.Mutex
	var beats []string
	c.OnDeliver(func(menu string) {
		mu.Lock()
		beats = append(beats, menu)
		mu.Unlock()
	})

	var got [][]routeros.Reply
	release := c.Subscribe("/interface/monitor-traffic", nil, 5*time.Millisecond,
		func(rows []routeros.Reply, _ error) {
			mu.Lock()
			got = append(got, rows)
			mu.Unlock()
		})
	defer release()

	p.push(routeros.Reply{"name": "ether1", "rx-bits-per-second": "100"})

	sched := NewScheduler(c, 2*time.Millisecond)
	sched.Start()
	defer sched.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n, d := len(got), len(beats)
		mu.Unlock()
		if n > 0 && d > 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(beats) == 0 {
		t.Error("a streamed menu never fired OnDeliver. Dormancy rides that hook, so " +
			"every streamed collector would go unjudged and nothing would ever sleep — " +
			"and nothing would fail to say so.")
	}
	if len(got) == 0 {
		t.Fatal("the subscriber's onRows never ran for a streamed menu")
	}
	if len(got[0]) != 1 || got[0][0]["rx-bits-per-second"] != "100" {
		t.Errorf("the subscriber received %v, want the pushed row", got[0])
	}
	if _, _, reads := p.counts(); reads != 0 {
		t.Errorf("the scheduler issued %d read(s) for a streamed menu", reads)
	}
}

// TestATableThatCanBeEmptyIsRefused.
//
// ── THE SECOND KIND, NARROWED BY B.6 ───────────────────────────────────────
//
// The first kind is rows that are not readings of one value: `ping`, `logs`.
//
// The second USED TO BE churn, because the entry could only accumulate and a row
// that left the table never left the map. B.6 found the round boundary, so that
// reason is gone and the connection table came off this list.
//
// What remains is narrower and real: a table with NO ROWS sends nothing, and
// nothing is indistinguishable from a dead stream — which the watchdog treats as
// death and reopens. So an EMPTIED table holds its last contents. Bounded by the
// next row rather than by the session, but still wrong, and these are the menus
// where empty is an ORDINARY state: a router with no associated clients and no
// PPP sessions is a completely normal router.
func TestATableThatCanBeEmptyIsRefused(t *testing.T) {
	c := New(&pusher{})
	churning := []string{
		"/interface/wifi/registration-table/print",
		"/interface/wireless/registration-table/print",
		"/ip/dhcp-server/lease/print",
		"/ppp/active/print",
		"/ip/kid-control/device/print",
	}
	for _, menu := range churning {
		stop, err := c.FillFromStream(menu, routeros.Cmd{Path: menu}, byName, time.Hour)
		if err == nil {
			stop()
			t.Errorf("%s was accepted for stream-filling. It can be legitimately EMPTY, "+
				"and an empty table sends nothing — which is indistinguishable from a "+
				"dead stream, so the entry would hold its last contents and the page "+
				"would show rows that have gone.", menu)
		}
	}
}
