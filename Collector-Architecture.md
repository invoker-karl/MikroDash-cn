# Collector Architecture

**What this is.** The current shape of MikroDash's collector layer, and why it is
that shape. Not a plan and not a history: it describes what the code does today.

**It is a living document, and it is gated.** `internal/verify/architecture_test.go`
re-measures every number and every collector name below, and fails in both
directions — a claim that has gone stale is a failure, and so is a collector that
exists and is not described here. That gate is the only reason to trust this file;
CLAUDE.md's most expensive recurring defect is a premise that expired with nothing
failing, and a document nobody re-measures is exactly that defect in prose.

**When you change the collector layer, change this file in the same commit.**

---

## The shape, in one picture

```
        RouterOS                              this process
   ┌──────────────┐
   │  a menu      │   1  ACQUISITION      internal/collect/*.go  (the reads)
   │  /ip/dns     │      one subscription per menu,             internal/roscache
   │  ...         │      one scheduler per router,              internal/roslimit
   └──────┬───────┘      one read shared by every asker
          │ rows
          ▼
   ╔══════════════╗   2  DERIVATION       BuildX(...) / FoldX(...)
   ║ pure function║      rows in, payload out. No I/O, no receiver,
   ╚══════┬═══════╝      callable from a test without a router
          │ payload
          ▼
   ┌──────────────┐   3  VIEWS            internal/collect/rooms.go
   │ page-dns     │      a collector declares its ROOMS; the hub knows
   │ dash-card-…  │      who is in each; demand decides what runs
   └──────────────┘      internal/server/demand.go
```

The three layers answer three different questions, and keeping them apart is what
lets each change without the others:

| layer | the question it answers | where it lives |
|---|---|---|
| **Acquisition** | *how do rows get here, and how often* | `internal/roscache`, `scheduled` in `internal/collect` |
| **Derivation** | *what do these rows mean* | `BuildX` / `FoldX` functions, pure |
| **Views** | *who is listening, and should this run at all* | `rooms.go`, `internal/server/demand.go` |

---

## Layer 1 — Acquisition

**Its job: get rows off the router as few times as possible.** The documented
bottleneck is concurrent API channels on the MikroTik, not CPU here — so
"efficient" means *fewer router channels*, never faster parsing.

### Set A and Set B

Every acquisition is one of two kinds, and the split decides everything else about
it. `KindOf` in `internal/collect/acquisition.go` is the classifier.

**Set A — a cacheable query.** A menu read that answers with the current contents
of a table. Rows are *successive readings of a keyed value*, so a later read
replaces an earlier one and a cache entry can stand for the menu.

**Set B — a measurement or a stream.** Rows are *distinct elements*, not readings
of one value: a ping result counted into loss, a log line, a traffic sample. No
cache can serve them, because there is no "current value" to hold.

That distinction is not stylistic. A rolling cache entry backed by set B rows
reports nonsense — 0% packet loss for ever, dropped log lines — and nothing fails.
`roscache.Unrollable` refuses the two menus where it would.

### How a set A read happens

1. A collector declares a **subscription**: a menu, a proplist and a cadence
   (`scheduled` in `internal/collect/scheduled.go`).
2. `internal/roscache` keeps the **demand set** — who wants which menu.
3. **One scheduler goroutine per router** decides when each menu is due, reads it
   once, and delivers to every subscriber. Two collectors on one menu with
   byte-identical proplists cost one read: `conns` and `bandwidth` share
   `/ip/firewall/connection/print` this way.
4. `internal/roslimit` caps commands in flight at 8 per router.

### Streams: a second filler for the same entry

A cache entry can be kept current by an open channel instead of a read —
`/print =interval=N`, which the router pushes. The entry is the same; only the
filler changes. **A menu absent from the table polls**, which is what makes this
reversible one collector at a time.

Two mechanisms make it safe:

- **Round detection.** A `/print =interval=N` re-prints the whole table with no
  separator between rounds. A repeated key, or a quiet gap, ends a round — without
  which the entry could only accumulate, and rows that left the table would pile
  up for the life of the session.
- **The empty-table rule.** An empty table sends nothing, which is
  indistinguishable from a dead channel. The watchdog already reopens a quiet
  channel, so *silence surviving a deliberate restart* is evidence of an empty
  table rather than a broken one.

A menu may be shared by two holders with different needs: `JoinStream` merges
their commands — the union of the interfaces, the finest interval — and fans each
row out to holders that asked for one. `/interface/monitor-traffic` is the case:
`ifStatus` wants a snapshot, `traffic` wants every packet, and they hold **one
channel** between them.

### The stream/poll duality

Every collector offers both delivery modes, and the operator's per-router
Stream/Poll setting chooses. **Both honour the same interval slider** — choosing
Poll must never silently mean slower. Two conditions are required to stream: this
project's judgement that the menu is safe (`session.streamableMenus`) *and* the
operator's setting. Either saying no means poll.

### Acquisitions that are not router reads

Two sources sit in this layer and read something else:

- **`arp`** reads `/ip/arp/print` like any table and emits nothing. Its whole
  output is an in-memory IP↔MAC index that four collectors join against.
- **`PTRCache`** asks *this process's resolver*, not the router, what an address
  calls itself — the last fallback for naming a device with no DHCP lease.

---

## Layer 2 — Derivation

**Its job: turn rows into a payload, and nothing else.** A derivation takes rows
(and, where a collector carries state between ticks, the prior state) and returns
the payload. It performs no I/O, holds no receiver, and can be called from a test
without building a collector or a router.

Two shapes, and the difference follows Set A / Set B exactly:

```go
// table     map over the current state
func BuildX(prior State, rows []routeros.Reply) (Payload, State)

// sequence  fold one element in
func FoldX(prior State, row routeros.Reply, now int64) (Sample, State)
```

`FoldPing`, `FoldLog` and `FoldTraffic` are the three folds — the three set B
acquisitions. Everything else is a map.

**The purity is load-bearing in one specific way that bit three times.** `append`
into a slice with spare capacity writes *through* to the caller's backing array,
and a ring that has been trimmed always has spare capacity. Every fold copies, and
each says so where it does.

**24 of 27 collectors have an extracted derivation.** The three without are
exactly the set B streams, whose derivation is per-pushed-row and lives in the
fold. `internal/verify/derivations_test.go` is the ledger, and it fails both ways.

---

## Layer 3 — Views

**Its job: decide who receives a payload, and therefore what runs at all.**

A collector declares its **rooms** in `internal/collect/rooms.go` — one line per
audience, `page-<key>` for a page and `dash-card-<name>` for a dashboard card.
Nothing else states the audience: an `emit` takes the declaration.

That declaration used to be written twice — once at the emit, once by hand in the
server's blur logic — and the two disagreed **five times**, each one a dashboard
card that silently stopped updating for anybody who had visited the owning page
and left.

### Demand

`internal/server/demand.go` holds the whole rule:

> a collector runs if anybody is in any room it declares

plus two consumers that occupy no room and never will: **alerting** (the rules are
not in a room) and the **non-viewer holds** (a session kept alive for history, for
the Devices page, or merely warm).

Every event that can change the answer re-asks it — a page focus, a page blur, a
card blur, a router switch, and a socket closing. A suspend waits out a grace
period and re-asks when it fires, so a page refresh does not stop and restart a
channel to save one second of polling.

### Rooms a collector does not emit to

`keepAliveFor` is the exception, and there are **3** entries. It exists for an
in-process dependency no emit can express:

- **`ifStatus`** is the rate source for five collectors, four of which live on
  pages it sends nothing to. Gating on its own audience alone would blank every
  throughput column on Bridges, VLANs, WAN and Bandwidth.
- **`dhcpLeases`** emits *router-wide*, so it has no guardable audience at all,
  while the DHCP and Connections pages render it directly.
- **`arp`** emits nothing whatsoever. Its rooms are its four consumers'.

---

## How the three fit together

One payload, end to end — the DNS page:

1. **Views.** A browser opens `/dns` and joins `router-<id>-page-dns`.
   `applyDemand` asks the rule of every collector; `dns` declares `page-dns`, so it
   is wanted, and `ResumeCollector("dns")` starts it.
2. **Acquisition.** `dns` subscribes to `/ip/dns/print` at its cadence. The
   router's scheduler reads it once — or a channel fills the entry, if this router
   streams — and delivers rows to every subscriber.
3. **Derivation.** `ParseDNSSettings` and `ParseStaticEntries` turn rows into a
   `DNSPayload`. No lock is held, nothing is emitted, and the same call in a test
   needs no router.
4. **Views again.** The collector emits the payload to `page-dns`, and the hub
   fans it out to whoever is in that room.
5. The browser closes the tab. `releaseRouter` leaves the rooms **first**, then
   re-asks demand — so the departing connection is no longer counted as its own
   audience — and `dns` is handed to a grace timer that suspends it if nobody
   comes back.

The layers touch only at declared seams: a subscription (rows come back), a return
value (a payload comes out), a room name (which decides whether step 2 happens at
all). Nothing in the derivation knows who is watching; nothing in the view layer
knows what a menu is.

---

## The gates that decide whether a collector runs

Three, layered rather than competing:

| gate | asks | where |
|---|---|---|
| **demand** | is anybody in a room it feeds, or does a hold need it | `internal/server/demand.go` |
| **enablement** | has the operator turned it off for this router | `Session.CollectorEnabled` |
| **dormancy** | has it reported nothing for long enough to back off | `internal/dormancy` |

`ResumeCollector` is the only place a collector starts, precisely so that a gate
which knows nothing about dormancy cannot undo it. **`roslimit`** sits underneath
all three, capping commands in flight per router.

Two gates were retired. The **page-room switchboard** — a hand-written
page→collector map — is replaced by demand. The **idle collector prune** is
subsumed by demand, which now hears about a closing socket. The session's idle
grace remains: it closes the router *connection*, which is a different question.

---

## Every collector

Acquisition is the subscribed menu; a leading `—` marks a set B stream. Views are
the rooms it emits to, and `—` means router-wide or nothing.

| collector | acquisition | derivation | views |
|---|---|---|---|
| `arp` | `/ip/arp/print` | `BuildARP` | — (none at all) |
| `bandwidth` | `/ip/firewall/connection/print` | `BuildBandwidth` | `page-bandwidth`, `dash-card-bandwidth` |
| `bridges` | `/interface/bridge/host/print` | `BuildBridgeRows` | `page-bridges` |
| `capsman` | `/interface/wifi/registration-table/print` | `BuildCapsmanView` | `page-capsman` |
| `conns` | `/ip/firewall/connection/print` | `BuildConns` | `page-connections`, `dash-card-connections` |
| `dhcpLeases` | `/ip/dhcp-server/lease/print` | `BuildLeases` | — (router-wide) |
| `dhcpNetworks` | `/ip/dhcp-server/network/print` | `BuildLanOverview` | `page-dhcp`, `dash-card-network` |
| `dns` | `/ip/dns/print` | `ParseDNSSettings`, `ParseStaticEntries` | `page-dns` |
| `firewall` | the table on screen | `BuildFirewallRule` | `page-firewall`, `dash-card-firewall` |
| `ifStatus` | `/interface/print` | `BuildIfStatus` | `page-interfaces`, `page-network-topology`, `dash-card-physports` |
| `logs` | — `/log/listen` | fold: `FoldLog` | `page-logs`, `dash-card-logs` |
| `netwatch` | `/tool/netwatch/print` | `BuildNetwatch` | `page-dashboard` |
| `packages` | `/system/package/print` | `BuildPackages` | `page-packages` |
| `ping` | — `/tool/ping` | fold: `FoldPing` | `page-dashboard` |
| `ppp` | `/ppp/active/print` | `ParsePPPSessions` | `page-ppp` |
| `queues` | `/queue/simple/print` | `BuildQueueRows` | `page-queues` |
| `rosusers` | `/user/print` | `BuildUsersView` | `page-users` |
| `routing` | `/routing/bgp/session/print` | `BuildRouting` | `page-routing`, `page-dashboard` |
| `system` | `/system/resource/print` | `buildSystem` | — (router-wide) |
| `talkers` | `/ip/kid-control/device/print` | `BuildTalkers` | `page-dashboard` |
| `topology` | `/ip/neighbor/print` | `BuildTopology` | `page-network-topology` |
| `traffic` | — `/interface/monitor-traffic` | fold: `FoldTraffic` | per-interface rooms |
| `vlans` | `/interface/vlan/print` | `BuildVlanRows` | `page-vlans` |
| `vpn` | `/ppp/active/print` | `ParsePppSessions`, `ParseIpsecPeers` | `page-vpn`, `dash-card-vpn` |
| `wan` | `/interface/detect-internet/state/print` | `BuildWanRows` | `page-wan` |
| `wifi` | `/interface/wifi/print` | `BuildWifiView` | `page-wifi-networks` |
| `wireless` | `/interface/wifi/registration-table/print` | `BuildWirelessView` | `page-wifi-clients`, `dash-card-wireless` |

**Two collectors have no `Start()`.** `packages` and `routing` are page-gated
only: the session brings them up with `Resume()` and nothing else. That is not a
quirk to tidy — it is what "page-gated" means in this design — but it did hide
them from a gate for a while, so it is written down.

**The in-process edges** — one collector reading another's output — are declared
as capabilities, never as a pointer to the producer: `RateSource`, `LeaseSource`,
`LeaseIPs`, `LeaseCounts`, `NetworkSource`, `SystemSource`, `FilterRowSource`,
`ARPByIP`, `ARPByMAC`, `NameByIP`. A consumer names the *question* it needs
answered, which is what lets it be tested without the producer, and what makes the
edge visible from the consumer's own type.
`internal/verify/collectoredges_test.go` is the ledger.

---

## Measured facts

The gate re-computes each of these. If one is wrong, the gate fails rather than
the document quietly lying.

| fact | value |
|---|---|
| registry rows | 27 |
| collectors with a Go implementation | 27 |
| disableable by the operator | 22 |
| dormancy-eligible | 19 |
| gated by demand (`session.TargetKeys`) | 25 |
| menus enabled for stream delivery | 14 |
| collectors declaring rooms | 23 |
| `keepAliveFor` entries | 3 |
| collectors with an extracted derivation | 24 |

---

## Changing this architecture

Adding or changing a collector touches all three layers, and each has a gate that
tells you if you missed one:

1. **Acquisition** — declare the subscription (`scheduled`), or record why it is
   set B. `internal/verify/scheduled_test.go`, `acquisition_test.go`.
2. **Derivation** — a package-level `BuildX`/`FoldX`, listed in
   `internal/verify/derivations_test.go`.
3. **Views** — rooms in `internal/collect/rooms.go`, or a `keepAliveFor` entry
   with its reason. `internal/server/demand_test.go` refuses a collector no room
   can ever want.
4. **Lifecycle** — `internal/session`: construct it, add it to the `UseCache`
   list, the dormancy target table, and the connect/reconnect/teardown blocks. The
   counts in `lifecycle_test.go` and `release_test.go` are pinned.
5. **This file** — `internal/verify/architecture_test.go` fails until the table
   and the numbers above match the code.
