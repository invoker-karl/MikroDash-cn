package collect

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"mikrodash/internal/routeros"
)

// fakePTR is a resolver a test controls: no DNS is ever touched, which is the
// reason `PTRCache.lookup` is a field at all.
type fakePTR struct {
	mu    sync.Mutex
	hosts map[string][]string
	err   error
	calls map[string]int
	// block, when non-nil, holds every lookup until it is closed. It is how the
	// in-flight test gets two callers into the same window.
	block chan struct{}
}

func newFakePTR(hosts map[string][]string) *fakePTR {
	return &fakePTR{hosts: hosts, calls: map[string]int{}}
}

func (f *fakePTR) lookup(_ context.Context, ip string) ([]string, error) {
	f.mu.Lock()
	f.calls[ip]++
	block, err, hosts := f.block, f.err, f.hosts[ip]
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, errors.New("no such host")
	}
	return hosts, nil
}

func (f *fakePTR) count(ip string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[ip]
}

func ptrWith(f *fakePTR) *PTRCache {
	c := NewPTRCache()
	c.lookup = f.lookup
	return c
}

// waitFor polls a condition rather than sleeping a fixed span: the lookup runs
// on its own goroutine, and a fixed sleep is the difference between a test that
// is slow and one that is flaky.
func waitFor(t *testing.T, why string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", why)
}

// TestPTRNameNeverBlocksAndArrivesLater is the whole shape of this thing.
//
// A DNS lookup can take seconds and a collector's tick may not, so the first
// read MUST return empty rather than wait — and the answer has to turn up
// afterwards or the fallback is decoration.
func TestPTRNameNeverBlocksAndArrivesLater(t *testing.T) {
	f := newFakePTR(map[string][]string{"10.0.0.9": {"printer.lan."}})
	c := ptrWith(f)

	if got := c.PTRName("10.0.0.9"); got != "" {
		t.Errorf("the first read returned %q; it must not wait for DNS", got)
	}
	c.WantPTR("10.0.0.9")
	waitFor(t, "the lookup to land", func() bool { return c.PTRName("10.0.0.9") != "" })
	if got := c.PTRName("10.0.0.9"); got != "printer" {
		t.Errorf("PTRName = %q, want the first label of printer.lan.", got)
	}
}

// TestFirstLabel is the live reduction: the page has a column for a device name
// and not for a FQDN.
func TestFirstLabel(t *testing.T) {
	for _, c := range []struct {
		in   []string
		want string
	}{
		{[]string{"printer.lan."}, "printer"},
		{[]string{"nas.home.arpa"}, "nas"},
		{[]string{"bare"}, "bare"},
		{[]string{" spaced.lan. "}, "spaced"},
		{[]string{"a.b.c.", "second.lan."}, "a"}, // the FIRST answer wins
		{nil, ""},
		{[]string{""}, ""},
	} {
		if got := firstLabel(c.in); got != c.want {
			t.Errorf("firstLabel(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestAFailedLookupIsCachedAndNotRetriedImmediately.
//
// THE DEPARTURE THIS PINS. The live negative TTL is 15s and is UNREACHABLE —
// `resolveName` returns any entry it finds regardless of age, so a failure was
// cached for the session. Ten minutes is close to that; fifteen seconds would
// send more DNS than the live app ever has, per unnamed client, for ever.
func TestAFailedLookupIsCachedAndNotRetriedImmediately(t *testing.T) {
	f := newFakePTR(nil) // every lookup fails
	c := ptrWith(f)

	c.WantPTR("10.0.0.9")
	waitFor(t, "the failed lookup to settle", func() bool { return f.count("10.0.0.9") == 1 })

	for range 5 {
		c.WantPTR("10.0.0.9")
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.count("10.0.0.9"); n != 1 {
		t.Errorf("%d lookups for an address that failed once; a miss must be cached, "+
			"or every unnamed client queries DNS on every tick for ever", n)
	}
	if got := c.PTRName("10.0.0.9"); got != "" {
		t.Errorf("a failed lookup produced the name %q", got)
	}
}

// TestConcurrentWantsIssueOneLookup — departure 2. The live code fires one
// lookup per caller, so two clients resolving at once queried twice.
func TestConcurrentWantsIssueOneLookup(t *testing.T) {
	f := newFakePTR(map[string][]string{"10.0.0.9": {"nas.lan."}})
	f.block = make(chan struct{})
	c := ptrWith(f)

	for range 8 {
		c.WantPTR("10.0.0.9")
	}
	waitFor(t, "the first lookup to start", func() bool { return f.count("10.0.0.9") >= 1 })
	close(f.block)
	waitFor(t, "the lookup to land", func() bool { return c.PTRName("10.0.0.9") != "" })

	if n := f.count("10.0.0.9"); n != 1 {
		t.Errorf("%d concurrent lookups for one address, want 1", n)
	}
}

// TestOnResolvedFiresForANameAndNotForAMiss.
//
// On a network with no reverse zone EVERY lookup misses, and a callback per miss
// would re-render the client list for an answer that has not changed.
func TestOnResolvedFiresForANameAndNotForAMiss(t *testing.T) {
	f := newFakePTR(map[string][]string{"10.0.0.9": {"nas.lan."}})
	c := ptrWith(f)
	var mu sync.Mutex
	var fired int
	c.OnResolved(func() { mu.Lock(); fired++; mu.Unlock() })

	c.WantPTR("10.0.0.9") // a hit
	waitFor(t, "the hit", func() bool { mu.Lock(); defer mu.Unlock(); return fired == 1 })

	c.WantPTR("10.0.0.77") // a miss
	waitFor(t, "the miss to settle", func() bool { return f.count("10.0.0.77") == 1 })
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if fired != 1 {
		t.Errorf("OnResolved fired %d times; a miss must not wake the page", fired)
	}
}

// TestPTRCacheIsBounded — departure 3. The live Map is unbounded and cleared
// only on reconnect.
func TestPTRCacheIsBounded(t *testing.T) {
	c := NewPTRCache()
	c.mu.Lock()
	for i := range ptrMax + 50 {
		c.entries[strings.Repeat("a", i%3)+itoaSmall(i)] = ptrEntry{name: "x", at: time.Now().Add(-time.Duration(i) * time.Second)}
	}
	c.evictLocked()
	n := len(c.entries)
	c.mu.Unlock()
	if n != ptrMax {
		t.Errorf("the cache holds %d entries after eviction, want the %d cap", n, ptrMax)
	}
}

func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ── THE CONSUMER ───────────────────────────────────────────────────────────

// TestWirelessNamesAClientFromReverseDNS drives a tick, because the helper is
// not the bug: a lookup that works with nothing calling it is the exact defect
// the ARP work was about, and it survived a first test pass there.
func TestWirelessNamesAClientFromReverseDNS(t *testing.T) {
	ros := fakeReader{rows: map[string][]routeros.Reply{
		"/interface/wifi/registration-table/print": {
			{"mac-address": "AA:BB:CC:DD:EE:FF", "signal": "-52", "interface": "wifi1"},
		},
	}}
	f := newFakePTR(map[string][]string{"10.0.0.9": {"printer.lan."}})
	ptr := ptrWith(f)
	c := NewWireless(ros, func(string, string, any) {}, nil, 30000).
		WithARP(stubARP{byMAC: map[string]string{"AA:BB:CC:DD:EE:FF": "10.0.0.9"}}).
		WithPTR(ptr)

	// The FIRST tick cannot have the name: the lookup has not happened. That is
	// not a defect, it is the contract — a tick must not wait on DNS.
	c.Tick()
	if got := c.Last(); got == nil || len(got.Clients) != 1 || got.Clients[0].Name != "" {
		t.Fatalf("the first tick blocked on DNS or invented a name: %+v", got)
	}
	// …and the answer must have been ASKED FOR, which an empty name alone
	// cannot distinguish from a collector that never tried.
	waitFor(t, "the lookup the tick asked for", func() bool { return f.count("10.0.0.9") == 1 })

	// The callback re-renders without another router read.
	waitFor(t, "the name to reach the payload", func() bool {
		p := c.Last()
		return p != nil && len(p.Clients) == 1 && p.Clients[0].Name == "printer"
	})
}

// TestALeaseNameBeatsAPTRRecord. A lease name is what the operator's own DHCP
// server was told; a PTR record is whatever is in a zone file. The lease wins,
// and reverse DNS is asked only for what is left.
func TestALeaseNameBeatsAPTRRecord(t *testing.T) {
	f := newFakePTR(map[string][]string{"10.0.0.9": {"wrong.lan."}})
	ptr := ptrWith(f)
	w := &Wireless{
		leases: stubLeases{&LeasesPayload{Leases: []Lease{
			{MAC: "AA:BB:CC:DD:EE:FF", Name: "FromLease"},
		}}},
		ptr: ptr,
	}
	if got := w.nameOf("AA:BB:CC:DD:EE:FF", "10.0.0.9"); got != "FromLease" {
		t.Errorf("nameOf = %q, want FromLease", got)
	}
	time.Sleep(30 * time.Millisecond)
	if n := f.count("10.0.0.9"); n != 0 {
		t.Errorf("%d DNS lookups for a client the lease already named", n)
	}
}

// TestWirelessSurvivesNoPTR — the capability is optional, like every other.
func TestWirelessSurvivesNoPTR(t *testing.T) {
	w := &Wireless{}
	if got := w.nameOf("AA:BB:CC:DD:EE:FF", "10.0.0.9"); got != "" {
		t.Errorf("with no leases and no PTR: %q", got)
	}
	w.renameFromPTR() // must not panic with no payload
}
