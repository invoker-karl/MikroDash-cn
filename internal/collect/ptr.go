package collect

// Reverse-DNS names — the port of `_ptrLookup` in src/collectors/wireless.js.
//
// ── THE LAST FALLBACK, AND IT ASKS SOMETHING NOBODY ELSE DOES ──────────────
//
// Every other name in this app comes from the router: a DHCP lease, a
// neighbour's identity, a bridge host. This one asks the RESOLVER THIS PROCESS
// USES — not the router — what a LAN address calls itself. It is the only thing
// that names a device with a static address and no lease, which on a real
// network is printers, servers and anything configured by hand.
//
// `wireless` is its only consumer, as in the live app: a registration row has a
// MAC and no name, the lease table answers most of them, and this answers some
// of the rest.
//
// ── IT NEVER BLOCKS A TICK ─────────────────────────────────────────────────
//
// A DNS lookup can take seconds and a collector's tick may not. So `PTRName` is
// a pure cache read that returns immediately, and `WantPTR` kicks off a lookup
// whose answer lands later. That is exactly the live shape — `resolveName`
// returns '' and calls `_ptrLookup` without awaiting it — and it is why
// `OnResolved` exists: something has to tell the page when the name arrives.
//
// ── FOUR DEPARTURES FROM LIVE, EACH DELIBERATE ─────────────────────────────
//
//  1. THE NEGATIVE TTL. The live constants are 60s for a hit and 15s for a miss,
//     and THE MISS ONE IS UNREACHABLE: `resolveName` returns `cached.name` for
//     any entry it finds regardless of age, so `_ptrLookup` only ever runs when
//     there is no entry at all, and a failed lookup was therefore cached for the
//     life of the session. Reproducing the 15s literally would send a query per
//     unnamed client every fifteen seconds — MORE DNS than the live app has ever
//     sent, in the name of matching it. So the hit TTL is the live 60s and the
//     miss TTL is ten minutes: far closer to what live actually did, while still
//     letting a device that gains a PTR record be found.
//
//  2. IN-FLIGHT DEDUPLICATION. Two clients resolving at once fired two lookups
//     for one address there; here the second joins the first.
//
//  3. A BOUND. The live cache is an unbounded Map cleared only on reconnect. A
//     long-lived session on a busy network grows it without limit, so this one
//     has a cap and drops the oldest entries when it is reached.
//
//  4. A TIMEOUT. `dns.reverse` inherits the system resolver's, which on a
//     network with no reverse zone can be seconds per query with several
//     retries. Two seconds is enough for a LAN resolver to answer or fail.

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// ptrHitTTL is the live 60s: a name may change, and this is how long before
	// it is asked again.
	ptrHitTTL = 60 * time.Second
	// ptrMissTTL is ten minutes. See departure 1 for why it is not the live 15s.
	ptrMissTTL = 10 * time.Minute
	// ptrTimeout bounds one lookup.
	ptrTimeout = 2 * time.Second
	// ptrMax bounds the cache. Well past any home or small-office network, and
	// small enough that a runaway cannot matter.
	ptrMax = 512
)

// NameByIP is the capability `wireless` declares: the last resort for a name.
//
// TWO METHODS, because the caller does two different things. `PTRName` reads
// what is known now and must not block; `WantPTR` says "I needed a name for this
// address and had none", which is what starts a lookup. Splitting them keeps the
// read side pure — a consumer that only renders can hold something that never
// issues a query.
type NameByIP interface {
	PTRName(ip string) string
	WantPTR(ip string)
}

type ptrEntry struct {
	name string
	at   time.Time
}

// PTRCache is one router session's reverse-DNS answers.
//
// PER SESSION, matching live, even though DNS answers are not per router: it is
// cleared when a router reconnects, and a shared cache would carry one router's
// stale answers onto another.
type PTRCache struct {
	// lookup is the resolver call, injected so a test never touches DNS.
	lookup func(ctx context.Context, ip string) ([]string, error)

	mu         sync.Mutex
	entries    map[string]ptrEntry
	inflight   map[string]bool
	onResolved func()
	// resolved counts completed lookups. Read by a test to know a lookup
	// happened at all, which "the name is empty" cannot distinguish from "no
	// lookup was made".
	resolved int
}

func NewPTRCache() *PTRCache {
	return &PTRCache{
		lookup: func(ctx context.Context, ip string) ([]string, error) {
			return net.DefaultResolver.LookupAddr(ctx, ip)
		},
		entries:  map[string]ptrEntry{},
		inflight: map[string]bool{},
	}
}

// OnResolved is called after a lookup lands with a NAME, never on a miss.
//
// ── WHY A CALLBACK AND NOT A RETRY TIMER ───────────────────────────────────
//
// The live app polls: after emitting a payload with unnamed clients it sets a
// 500ms timer, re-resolves them, re-emits if anything changed, and reschedules
// while any lookup is outstanding. That is a timer, a fingerprint comparison and
// a termination condition to get wrong, and it delivers on average 250ms late.
//
// The answer arriving IS the event. `wireless` re-renders from its last payload
// when this fires, which is the same visible behaviour with none of the
// machinery — and it cannot spin, because a callback happens once per lookup.
//
// Never called under this type's lock: the consumer takes its own.
func (p *PTRCache) OnResolved(fn func()) {
	p.mu.Lock()
	p.onResolved = fn
	p.mu.Unlock()
}

// PTRName is the cached name for an address, or "".
func (p *PTRCache) PTRName(ip string) string {
	if p == nil || ip == "" {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[ip]
	if !ok || expiredLocked(e) {
		return ""
	}
	return e.name
}

// WantPTR starts a lookup for an address whose name is not known.
//
// A NO-OP when the answer is cached and fresh, or when one is already in flight.
// So a consumer may call it on every row of every tick, which is what makes the
// read side able to stay pure.
func (p *PTRCache) WantPTR(ip string) {
	if p == nil || ip == "" || p.lookup == nil {
		return
	}
	p.mu.Lock()
	if p.inflight[ip] {
		p.mu.Unlock()
		return
	}
	if e, ok := p.entries[ip]; ok && !expiredLocked(e) {
		p.mu.Unlock()
		return
	}
	p.inflight[ip] = true
	p.mu.Unlock()

	go p.resolve(ip)
}

func expiredLocked(e ptrEntry) bool {
	ttl := ptrMissTTL
	if e.name != "" {
		ttl = ptrHitTTL
	}
	return time.Since(e.at) > ttl
}

func (p *PTRCache) resolve(ip string) {
	ctx, cancel := context.WithTimeout(context.Background(), ptrTimeout)
	defer cancel()
	hosts, err := p.lookup(ctx, ip)

	name := ""
	if err == nil {
		name = firstLabel(hosts)
	}

	p.mu.Lock()
	delete(p.inflight, ip)
	p.entries[ip] = ptrEntry{name: name, at: time.Now()}
	p.resolved++
	p.evictLocked()
	fn := p.onResolved
	p.mu.Unlock()

	// A MISS TELLS THE PAGE NOTHING, so it does not wake it. On a network with
	// no reverse zone every lookup misses, and a callback per miss would
	// re-render the client list for an answer that has not changed.
	if name != "" && fn != nil {
		fn()
	}
}

// firstLabel is the live reduction: take the first answer, drop the trailing
// dot, and keep only the leftmost label — `printer.lan.` becomes `printer`,
// because the page has a column for a device name and not for a FQDN.
func firstLabel(hosts []string) string {
	if len(hosts) == 0 {
		return ""
	}
	h := strings.TrimSuffix(strings.TrimSpace(hosts[0]), ".")
	if i := strings.Index(h, "."); i >= 0 {
		h = h[:i]
	}
	return h
}

// evictLocked drops the oldest entries once the cap is passed. Caller holds mu.
//
// OLDEST BY WHEN THEY WERE ANSWERED, not by use: an entry's whole value is its
// freshness, and the oldest is the one closest to being re-asked anyway.
func (p *PTRCache) evictLocked() {
	if len(p.entries) <= ptrMax {
		return
	}
	type aged struct {
		ip string
		at time.Time
	}
	all := make([]aged, 0, len(p.entries))
	for ip, e := range p.entries {
		all = append(all, aged{ip, e.at})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for _, a := range all[:len(all)-ptrMax] {
		delete(p.entries, a.ip)
	}
}

// Reset drops everything, for a reconnect. The router that came back may be a
// different one, or the same one after a DHCP sweep.
func (p *PTRCache) Reset() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.entries = map[string]ptrEntry{}
	p.inflight = map[string]bool{}
	p.mu.Unlock()
}
