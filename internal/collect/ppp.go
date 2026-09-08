package collect

// PPP collector — the port of src/collectors/ppp.js (issue #32, "Live PPPoE
// Metrics").
//
//	/ppp/active                     the sessions
//	/ppp/profile                    the profiles they were assigned
//	/interface/pppoe-server/server  the PPPoE servers that accept them
//	/ppp/secret                     the accounts themselves, MINUS THE PASSWORD
//
// ── THE PASSWORD IS NEVER READ. THE REST OF THE MENU NOW IS ─────────────────
//
// This file used to say "/ppp/secret IS NEVER READ", and the reason recorded
// with it (CHANGELOG, issue #64) was two claims joined by "and":
//
//	it holds credentials, and the active list already carries everything
//	worth showing.
//
// Only the first survives. The second was true of a MONITORING page and is
// false of a MANAGEMENT one — managing subscribers is precisely the thing the
// active list cannot do (issue #125).
//
// So the rule is NARROWED, not reversed. `pppSecretCmd` names every property
// this page needs and does NOT name `password`, and that absence is the whole
// security property: a password cannot reach a browser through a payload it was
// never read into. Writes go the other way entirely, through
// `internal/resource`'s `TypeSecret`, which is write-only by construction.
//
// `TestNoProplistNamesACredential` enforces this now. It is worth knowing that
// the sentence this replaces claimed "a test enforces it across both" — that
// test was the Node original's and went at cutover, so the rule spent the whole
// port with nothing holding it up.
//
// ── WHAT THIS DOES NOT CLAIM ────────────────────────────────────────────────
//
// The property is "no password reaches a BROWSER", not "no password is ever
// read". The write path is a separate road: `internal/server`'s `readMenu`
// prints the whole menu with NO proplist — deliberately, because `ReadOnlyWhen`
// needs properties no page asks for — so a save, a delete or an enable does pull
// cleartext passwords into server memory for the length of that call. They stop
// there: `RowValues` drops every secret-typed field, `PreviewCommand` masks it,
// and the audit trail masks it by type. Narrowing that read too is a change to
// shared machinery and belongs in its own piece of work; what must not happen is
// this comment being read as covering it.
//
// ── RATES ARE DERIVED, AND null IS NOT ZERO ──────────────────────────────────
//
// RouterOS reports cumulative bytes only, so per-user bandwidth — the actual ask
// in #32 — comes from differencing two readings. The FIRST sample of a session
// therefore has no rate at all, and that is reported as null rather than 0:
// there is no measurement window yet, and 0 would claim an idle session that may
// be saturating the line.
//
// Two further rules, both carried over intact:
//
//   - the rate is clamped at 0, because a session that reconnects restarts its
//     counters and a negative rate is worse than a missed sample;
//   - the baseline timestamp advances ONLY when the bytes actually moved, so the
//     window always spans a real interval even when polls land between counter
//     updates. Bytes unchanged for longer than the idle threshold then read as
//     idle rather than as "still at the last rate".
//
// ── NOT VERIFIED AGAINST HARDWARE ────────────────────────────────────────────
//
// The Node header says so and it is still true here: the fleet runs no PPP, so
// /ppp/active returns the empty-menu junk row and every session-shaped field
// comes from the RouterOS field reference and the fixture. The EMPTY state is
// the only part real hardware has exercised — on either side.

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mikrodash/internal/roscache"
	"mikrodash/internal/routeros"
)

var (
	pppActiveCmd = routeros.Cmd{Path: "/ppp/active/print", Args: []string{
		"=.proplist=.id,name,service,caller-id,address,uptime,encoding,session-id," +
			"limit-bytes-in,limit-bytes-out,bytes-in,bytes-out"}}
	pppProfileCmd = routeros.Cmd{Path: "/ppp/profile/print", Args: []string{
		"=.proplist=.id,name,local-address,remote-address,rate-limit,only-one,use-encryption"}}
	pppServerCmd = routeros.Cmd{Path: "/interface/pppoe-server/server/print", Args: []string{
		"=.proplist=.id,service-name,interface,disabled,max-sessions,authentication"}}
	// EVERY PROPERTY THE PAGE NEEDS, AND NOT `password`.
	//
	// The proplist is the enforcement point, not a convention: an explicit list
	// is the difference between "we chose not to show it" and "we never asked
	// for it". `/ppp/secret` also carries `remote-ipv6-prefix`, which nothing
	// renders yet and which is therefore left out rather than carried unused.
	pppSecretCmd = routeros.Cmd{Path: "/ppp/secret/print", Args: []string{
		"=.proplist=.id,name,service,profile,local-address,remote-address,caller-id," +
			"routes,limit-bytes-in,limit-bytes-out,comment,disabled"}}
)

// Config is re-read every N ticks; sessions are read every tick.
const pppConfigEvery = 12

// pppHeartbeat bounds how long the dirty check may stay silent.
//
// ── A CARD WITH NOTHING TO SAY WAS BEING CALLED STALE ──────────────────────
//
// The emit is gated on a fingerprint, so an unchanged tick sends nothing. That
// is right for bandwidth and wrong for LIVENESS, because the browser measures
// staleness as "how long since a payload arrived" and cannot tell a collector
// that is quiet from one that has stopped.
//
// The two contracts only conflict where the data can be genuinely static, and
// PPP is exactly that: a router with no sessions and no configuration changes
// produces an identical fingerprint for ever. So the first frame arrived, the
// timer ran, and 25 seconds later the Active Sessions card wore a "stale" badge
// while the collector was polling perfectly happily. Reported from the live
// install, on the routers that run no PPP at all.
//
// FIFTEEN SECONDS IS NOT ARBITRARY. `web/src/stale.ts` retunes each card's
// threshold to `pollMs + STALE_GRACE`, and STALE_GRACE is 20 s, so the smallest
// threshold this collector can face is its 2 s poll floor plus 20 s. A heartbeat
// below that is safe at every interval `clampPoll` allows, and 15 s clears the
// tightest case with room to spare. A slower poll simply emits on every tick,
// which is what it did before the dirty check existed.
//
// This does NOT make the fingerprint pointless: between heartbeats an unchanged
// tick still sends nothing, which on a 2 s poll is seven frames saved in eight.
const pppHeartbeat = 15 * time.Second

// Bytes unchanged for longer than this means idle, not "still at the last rate".
const pppIdleAfterSec = 10.0

// PPPSession is one row of /ppp/active as the page renders it.
type PPPSession struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Service   string   `json:"service"`
	Address   string   `json:"address"`
	CallerID  string   `json:"callerId"`
	Uptime    string   `json:"uptime"`
	Encoding  string   `json:"encoding"`
	SessionID string   `json:"sessionId"`
	LimitIn   *int     `json:"limitIn"`
	LimitOut  *int     `json:"limitOut"`
	RX        int      `json:"rx"`
	TX        int      `json:"tx"`
	RXRate    *float64 `json:"rxRate"`
	TXRate    *float64 `json:"txRate"`
}

// PPPSecret is one row of /ppp/secret — an ACCOUNT, not a session.
//
// THERE IS NO PASSWORD FIELD, AND THERE MUST NEVER BE ONE. See the header: the
// proplist does not ask for it, so there is nothing here to hold. The edit form
// writes one through `resource.TypeSecret`, which is write-only in the other
// direction.
//
// `Connected` is NOT read from the router. It is joined onto each account at
// emit time from the /ppp/active names — see Tick for why that is not done when
// the secrets are read.
type PPPSecret struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Service       string `json:"service"`
	Profile       string `json:"profile"`
	LocalAddress  string `json:"localAddress"`
	RemoteAddress string `json:"remoteAddress"`
	CallerID      string `json:"callerId"`
	Routes        string `json:"routes"`
	LimitIn       *int   `json:"limitIn"`
	LimitOut      *int   `json:"limitOut"`
	Comment       string `json:"comment"`
	Disabled      bool   `json:"disabled"`
	Connected     bool   `json:"connected"`
}

// ID IS CARRIED ON THE PROFILE AND NOT ON THE SERVER, which is the difference
// between the two tables rather than an oversight. `.id` was requested and
// dropped for both while they were read-only; the edit form addresses a row by
// it, so a profile now keeps it. A PPPoE server stays read-only — see the plan:
// changing one can cut the operator's own management path, which needs a guard
// nothing here provides — so carrying its id would be exactly the unused field
// the proplist comment above refuses to carry.
type PPPProfile struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	LocalAddress  string `json:"localAddress"`
	RemoteAddress string `json:"remoteAddress"`
	RateLimit     string `json:"rateLimit"`
	OnlyOne       string `json:"onlyOne"`
	Encryption    string `json:"encryption"`
}

type PPPServer struct {
	ServiceName string `json:"serviceName"`
	Interface   string `json:"interface"`
	MaxSessions string `json:"maxSessions"`
	Auth        string `json:"auth"`
	Disabled    bool   `json:"disabled"`
}

type PPPPayload struct {
	TS          int64          `json:"ts"`
	PollMs      int            `json:"pollMs"`
	Sessions    []PPPSession   `json:"sessions"`
	Secrets     []PPPSecret    `json:"secrets"`
	Profiles    []PPPProfile   `json:"profiles"`
	Servers     []PPPServer    `json:"servers"`
	ByService   map[string]int `json:"byService"`
	TotalRXRate *float64       `json:"totalRxRate"`
	TotalTXRate *float64       `json:"totalTxRate"`
	// So the page can say "this router has no PPP service" rather than showing
	// an empty table, which reads as a failure.
	Available bool `json:"available"`
}

// pppSample is the previous reading of one session's counters.
type pppSample struct {
	rx, tx int
	ts     time.Time
}

type PPP struct {
	ros    Reader
	emit   Emit
	poll   *pollLoop
	pollMs *pollInterval
	// See scheduled.go: subscribes to /ppp/active, the live sessions.
	sched scheduled
	// cache coalesces reads shared with another collector; nil outside a live
	// session, which is every test. See collect/cache.go.
	cache *roscache.Cache

	mu       sync.Mutex
	prev     map[string]pppSample
	sessions []PPPSession
	secrets  []PPPSecret
	profiles []PPPProfile
	servers  []PPPServer
	ticks    int
	lastFP   string
	lastEmit time.Time
	last     *PPPPayload
	// nil = unprobed, false = this router has no such menu, stop asking.
	activeAvail  *bool
	profileAvail *bool
	serverAvail  *bool
	secretAvail  *bool
}

func NewPPP(ros Reader, emit Emit, pollMs int) *PPP {
	// The Node signature is clampPoll(raw, def, hi, lo) and the call is
	// (pollMs, 5000, 60000, 2000). Reordered for this side's (raw, def, lo, hi).
	ms := clampPoll(pollMs, 5000, 2000, 60000)
	p := &PPP{ros: ros, emit: emit, pollMs: newPollInterval(ms), prev: map[string]pppSample{}}
	p.poll = newPollLoop(func() { p.Tick() },
		func() time.Duration { return time.Duration(ms) * time.Millisecond })
	// AFTER the loop: `scheduled` holds it as the no-cache fallback.
	p.sched = scheduled{loop: p.poll, menu: pppActiveCmd.Path, apply: p.apply,
		cadence: func() time.Duration { return time.Duration(ms) * time.Millisecond }}
	return p
}

// pppInt is the original's `_int`: parseInt on the string form, 0 when that is
// not finite. parseInt takes a LEADING number, so "100k" is 100 and "" is 0.
func pppInt(v string) int {
	s := strings.TrimSpace(v)
	end := 0
	if end < len(s) && (s[end] == '-' || s[end] == '+') {
		end++
	}
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0
	}
	return n
}

// pppLimit is a byte cap as the page renders it: ABSENT is nil, and a present
// zero is a real zero.
//
// RouterOS uses 0 for "no limit", so the two cannot be collapsed — nil means the
// router did not report the property at all, and the page draws a dash for one
// and "0" for the other. Extracted from ParsePPPSessions when /ppp/secret grew
// the same pair of properties; the behaviour is unchanged in both callers.
func pppLimit(v string) *int {
	if v == "" {
		return nil
	}
	n := pppInt(v)
	return &n
}

// read fetches one menu, latching the flag off when the router says the menu
// does not exist.
//
// LATCHED OFF DELIBERATELY: a router without PPP should be asked once, not every
// five seconds for ever. Any other error leaves the flag alone, because a
// timeout is not evidence that the menu is absent.
func (p *PPP) read(cmd routeros.Cmd, flag **bool) []routeros.Reply {
	if *flag != nil && !**flag {
		return nil
	}
	// THROUGH THE CACHE: `vpn` reads /ppp/active too, and reads it with no
	// proplist, so the union on that menu widens to the whole row. The other
	// three menus here have this collector alone and are unaffected.
	rows, err := readVia(p.cache, p.ros, cmd, p.pollMs.duration())
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no such") || strings.Contains(msg, "unknown command") {
			no := false
			*flag = &no
		}
		return nil
	}
	yes := true
	*flag = &yes
	out := make([]routeros.Reply, 0, len(rows))
	for _, r := range rows {
		if len(r) > 0 {
			out = append(out, r)
		}
	}
	return out
}

// ParsePPPSessions turns /ppp/active rows into sessions with derived rates.
//
// Exported because it is the whole of the interesting arithmetic and deserves
// testing without a router — the same split the Node file makes by hanging
// parsePppSessions off the class.
func ParsePPPSessions(rows []routeros.Reply, prev map[string]pppSample, now time.Time) []PPPSession {
	out := make([]PPPSession, 0, len(rows))
	live := map[string]bool{}

	for _, r := range rows {
		// Drops the {undefined:''} row RouterOS returns for an empty menu.
		if r["name"] == "" {
			continue
		}
		rx := pppInt(r["bytes-in"])
		tx := pppInt(r["bytes-out"])
		key := r[".id"]
		if key == "" {
			key = r["name"] + "/" + r["service"]
		}
		live[key] = true

		var rxRate, txRate *float64
		pr, seen := prev[key]
		if seen && now.After(pr.ts) {
			dtSec := now.Sub(pr.ts).Seconds()
			rr := max(0, float64(rx-pr.rx)/dtSec)
			tr := max(0, float64(tx-pr.tx)/dtSec)
			if rx == pr.rx && tx == pr.tx && dtSec > pppIdleAfterSec {
				rr, tr = 0, 0
			}
			rxRate, txRate = &rr, &tr
		}
		// Only advance the timestamp when the bytes actually moved, so the
		// window always spans a real interval.
		if !seen || rx != pr.rx || tx != pr.tx {
			prev[key] = pppSample{rx: rx, tx: tx, ts: now}
		}

		limitIn, limitOut := pppLimit(r["limit-bytes-in"]), pppLimit(r["limit-bytes-out"])

		out = append(out, PPPSession{
			ID: r[".id"], Name: r["name"],
			Service:   strings.ToUpper(r["service"]),
			Address:   r["address"],
			CallerID:  r["caller-id"],
			Uptime:    r["uptime"],
			Encoding:  r["encoding"],
			SessionID: r["session-id"],
			LimitIn:   limitIn, LimitOut: limitOut,
			RX: rx, TX: tx, RXRate: rxRate, TXRate: txRate,
		})
	}
	for k := range prev {
		if !live[k] {
			delete(prev, k)
		}
	}
	// localeCompare, not a byte sort — the same ordering every other table here
	// uses for a name column.
	sort.SliceStable(out, func(i, j int) bool { return Collate(out[i].Name, out[j].Name) < 0 })
	return out
}

func (p *PPP) loadConfig() {
	profiles := make([]PPPProfile, 0)
	for _, r := range p.read(pppProfileCmd, &p.profileAvail) {
		if r["name"] == "" {
			continue
		}
		profiles = append(profiles, PPPProfile{
			ID: r[".id"], Name: r["name"], LocalAddress: r["local-address"],
			RemoteAddress: r["remote-address"], RateLimit: r["rate-limit"],
			OnlyOne: r["only-one"], Encryption: r["use-encryption"],
		})
	}
	servers := make([]PPPServer, 0)
	for _, r := range p.read(pppServerCmd, &p.serverAvail) {
		if r["interface"] == "" && r["service-name"] == "" {
			continue
		}
		servers = append(servers, PPPServer{
			ServiceName: r["service-name"], Interface: r["interface"],
			MaxSessions: r["max-sessions"], Auth: r["authentication"],
			Disabled: boolOf(r["disabled"]),
		})
	}
	// SECRETS RIDE THE CONFIG CADENCE, not the tick. A subscriber list does not
	// change every five seconds, and CLAUDE.md's measure of efficiency is router
	// channels rather than CPU — so this is one command a minute, not twelve.
	// A write does not wait for it: `RefreshNow` re-reads immediately.
	secrets := make([]PPPSecret, 0)
	for _, r := range p.read(pppSecretCmd, &p.secretAvail) {
		if r["name"] == "" {
			continue
		}
		secrets = append(secrets, PPPSecret{
			ID: r[".id"], Name: r["name"], Service: r["service"],
			Profile: r["profile"], LocalAddress: r["local-address"],
			RemoteAddress: r["remote-address"], CallerID: r["caller-id"],
			Routes: r["routes"], Comment: r["comment"],
			LimitIn: pppLimit(r["limit-bytes-in"]), LimitOut: pppLimit(r["limit-bytes-out"]),
			Disabled: boolOf(r["disabled"]),
		})
	}
	// The same ordering the sessions table uses for a name column.
	sort.SliceStable(secrets, func(i, j int) bool {
		return Collate(secrets[i].Name, secrets[j].Name) < 0
	})
	p.profiles, p.servers, p.secrets = profiles, servers, secrets
}

func (p *PPP) Tick() {
	if !p.ros.Connected() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.ticks%pppConfigEvery == 0 {
		p.loadConfig()
	}
	p.ticks++
	p.applyLocked(p.read(pppActiveCmd, &p.activeAvail))
}

// apply is what the scheduler calls with the active sessions, this collector's
// live menu. The secrets, profiles and PPPoE servers keep their config cadence
// here -- see scheduled.go on why a collector subscribes to ONE menu.
func (p *PPP) apply(rows []routeros.Reply, err error) {
	if !p.ros.Connected() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	if err != nil {
		// The latch `read` would have set, derived from the scheduler's error.
		if menuMissing(err) {
			no := false
			p.activeAvail = &no
		}
		return
	}
	if p.activeAvail == nil {
		yes := true
		p.activeAvail = &yes
	}
	if p.ticks%pppConfigEvery == 0 {
		p.loadConfig()
	}
	p.ticks++
	p.applyLocked(rows)
}

// applyLocked builds and emits. The caller holds the lock.
func (p *PPP) applyLocked(rows []routeros.Reply) {
	p.sessions = ParsePPPSessions(rows, p.prev, time.Now())

	byService := map[string]int{}
	for _, s := range p.sessions {
		k := s.Service
		if k == "" {
			k = "OTHER"
		}
		byService[k]++
	}
	// Totals over the sessions that HAVE a rate. All-null means null, not zero:
	// the distinction between "nothing is flowing" and "we cannot say yet" is
	// the whole reason the per-session rates are nullable.
	var totalRX, totalTX *float64
	known := 0
	var sumRX, sumTX float64
	for _, s := range p.sessions {
		if s.RXRate != nil {
			known++
			sumRX += *s.RXRate
			sumTX += *s.TXRate
		}
	}
	if known > 0 {
		totalRX, totalTX = &sumRX, &sumTX
	}

	// ── CONNECTED IS JOINED HERE, NOT WHERE THE SECRETS WERE READ ──────────
	//
	// The two halves move at different speeds: sessions every tick, secrets once
	// every `pppConfigEvery` ticks. Setting `Connected` when the secrets are read
	// would freeze the pill for up to a minute — an account that dialled in four
	// seconds ago would read as offline, which is exactly the question the column
	// exists to answer.
	//
	// Joining at emit time costs no extra command and makes the pill as live as
	// the session table beside it. A FRESH SLICE each tick, because `p.secrets`
	// is the cached read: writing `Connected` into it would leave last tick's
	// answer behind on the next one.
	active := make(map[string]bool, len(p.sessions))
	for _, s := range p.sessions {
		active[s.Name] = true
	}
	secrets := make([]PPPSecret, len(p.secrets))
	for i, s := range p.secrets {
		s.Connected = active[s.Name]
		secrets[i] = s
	}

	payload := &PPPPayload{
		TS: time.Now().UnixMilli(), PollMs: p.pollMs.ms(),
		Sessions: p.sessions, Secrets: secrets, Profiles: p.profiles, Servers: p.servers,
		ByService: byService, TotalRXRate: totalRX, TotalTXRate: totalTX,
		Available: p.activeAvail == nil || *p.activeAvail,
	}
	p.last = payload

	var fp strings.Builder
	for _, s := range p.sessions {
		fp.WriteString(s.ID + "|" + s.Name + "|" + s.Service + "|" + s.Address + "|" +
			strconv.Itoa(s.RX) + "|" + strconv.Itoa(s.TX) + ";")
	}
	// ── THE CONFIG TABLES ARE FINGERPRINTED BY CONTENT, NOT BY COUNT ───────
	//
	// This counted profiles and servers, which was survivable while both were
	// read-only: nothing could change a row without adding or removing one. The
	// moment a profile can be EDITED that becomes a silent hole — change a
	// rate-limit, the count is identical, the fingerprint matches, and the frame
	// is suppressed. The operator saves and the table does not move.
	for _, s := range secrets {
		fp.WriteString(s.ID + "|" + s.Name + "|" + s.Service + "|" + s.Profile + "|" +
			s.LocalAddress + "|" + s.RemoteAddress + "|" + s.Comment + "|" +
			strconv.FormatBool(s.Disabled) + "|" + strconv.FormatBool(s.Connected) + ";")
	}
	for _, pr := range p.profiles {
		fp.WriteString(pr.ID + "|" + pr.Name + "|" + pr.LocalAddress + "|" +
			pr.RemoteAddress + "|" + pr.RateLimit + "|" + pr.OnlyOne + "|" +
			pr.Encryption + ";")
	}
	for _, sv := range p.servers {
		fp.WriteString(sv.ServiceName + "|" + sv.Interface + "|" +
			sv.MaxSessions + "|" + sv.Auth + "|" + strconv.FormatBool(sv.Disabled) + ";")
	}
	fp.WriteString("|" + strconv.FormatBool(payload.Available))
	// CHANGED, OR THE HEARTBEAT IS DUE. See pppHeartbeat: suppressing an
	// unchanged frame is right, suppressing them all is what made an idle
	// router's card claim to be stale.
	now := time.Now()
	if fp.String() == p.lastFP && now.Sub(p.lastEmit) < pppHeartbeat {
		return
	}
	p.lastFP = fp.String()
	p.lastEmit = now
	p.emit("page-ppp", "ppp:update", payload)
}

func (p *PPP) Last() *PPPPayload {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// UseCache feeds BOTH halves: the 1.4 shared-read cache and the subscription.
// Same cache, two uses.
func (p *PPP) UseCache(rc *roscache.Cache) {
	p.cache = rc
	p.sched.useCache(rc)
}

func (p *PPP) Start() {
	if !p.sched.scheduling() {
		p.Tick()
	}
	p.sched.begin()
}

// Reconnected clears the rate baseline with everything else: a reconnect may be
// a different router, and session byte counters restart in any case.
func (p *PPP) Reconnected() {
	p.sched.end()
	p.mu.Lock()
	clear(p.prev)
	p.lastFP = ""
	p.ticks = 0
	p.activeAvail, p.profileAvail, p.serverAvail, p.secretAvail = nil, nil, nil, nil
	p.mu.Unlock()
	if !p.sched.scheduling() {
		p.Tick()
	}
	p.sched.begin()
}

// RefreshNow re-reads everything at once, including the config tables.
//
// `ticks = 0` is what makes that true: the config menus — profiles, servers and
// the secrets — are read only when `ticks%pppConfigEvery == 0`, so a plain Tick
// would return the same subscriber list the write just changed. This is called
// from the resource write path, which is the one moment the slow tables are
// known to be stale.
func (p *PPP) RefreshNow() {
	if !p.ros.Connected() {
		return
	}
	p.mu.Lock()
	p.ticks = 0
	p.mu.Unlock()
	p.Tick()
}

func (p *PPP) Suspend() { p.sched.end() }
func (p *PPP) Resume()  { p.sched.begin() }
func (p *PPP) Stop() {
	p.sched.end()
	p.mu.Lock()
	p.lastFP = ""
	p.mu.Unlock()
}

// SetPollMs applies a new poll period to a running collector.
// See `System.SetPollMs` for why both halves are needed.
func (p *PPP) SetPollMs(ms int) {
	p.pollMs.set(ms)
	p.poll.retime()
}
