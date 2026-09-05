# loop.md — code review of `a8f3774`, item by item

Source: `/code-review` on commit `a8f3774` ("Devices: fill the blank card the reporting
toggle left behind"). Ten finder angles, 19 candidates, each verified by an independent
agent; 2 refuted and dropped. 15 items survived, listed here in the order they will be
worked.

Every claim below was re-read against the source before this file was written — the line
numbers are current, not the reviewer's.

**Rules for this loop** (from CLAUDE.md): append to `Changes.md` after every file edit;
commit after each item; never push. A check is never deleted to make a change quiet. If an
item turns out to be wrong on closer reading, it is marked REFUTED with the evidence, not
silently dropped.

---

## Status

| # | Item | File | Verdict | State |
|---|---|---|---|---|
| 1 | Mixed-connection row: merge ignores `Connected` | `internal/server/devices.go:212` | bug | todo |
| 2 | Prime placement creates the window item 1 patches | `internal/server/devices.go:612` | design | todo |
| 3 | Prime costs TWO commands, not one | `internal/alertpool/prime.go:113` | efficiency | todo |
| 4 | Prime goroutine unbounded: no timeout, no in-flight guard | `internal/alertpool/prime.go:68` | leak | todo |
| 5 | "the next snapshot two seconds later carries it" is false | `internal/alertpool/prime.go:16` | comment | todo |
| 6 | `PrimeStats` blocks the WebSocket reader | `internal/server/devices.go:612` | latency | todo |
| 7 | `-race` fails on `historyOn` (pre-existing, `d8fe2f8`) | `internal/routers/pool.go:717` | bug | todo |
| 8 | "primed N/M" tally counts stale values | `internal/alertpool/prime.go:94` | diagnostic | todo |
| 9 | Ordering pin is text-scanning and passes vacuously | `internal/verify/prime_test.go:52` | test | todo |
| 10 | No test can fail on item 1 or item 3 | `internal/server/devices_test.go:862` | test | todo |
| 11 | Session that goes collector-less after focus is never primed | `internal/server/server.go:427` | gap | todo |
| 12 | Comments the diff left contradicted | `internal/alertpool/pool.go:299` | comment | todo |
| 13 | Test fake panics if anyone adds `t.Cleanup(p.Close)` | `internal/alertpool/prime_test.go:132` | test | todo |
| 14 | `primedMu` redundant; two-pass count | `internal/alertpool/pool.go:131` | simplify | todo |
| 15 | Doc drift: unpinned prose, stale CONTRIBUTING | `CLAUDE.md:451` | docs | todo |

---

## 1. Mixed-connection row — `internal/server/devices.go:212`

`fillFromAlertPool`'s Known-branch copies `snap.System` / `snap.IfStatus` onto a summary
whose `Connected` and `LastError` came from a **different connection**, with no `Connected`
check on either side.

- `BuildRow` (`internal/routers/stats.go:166-169`) renders the login-failure box from
  `!in.Connected && in.LastError != ""`, and the gauges from `in.System != nil`,
  independently. So the row can show an Offline badge and a live CPU gauge in one frame.
- This is the exact mixing `internal/routers/assemble.go:142-144` calls out as ONE SOURCE
  PER ROW, and it contradicts `fillFromAlertPool`'s own header ("nothing here can mix two
  connections' readings into one card", `devices.go:144-145`).

**Reproduction:** router password rotated, or a transient dial timeout. The overview dial
fails, so the summary is `Known:true, Connected:false, LastError:"..."`; the alert-pool
socket dialled earlier is still up and primed.

**Fix:** fill only when both sides agree the router is up.

**Verify:** a test that sets `Connected:false` + `LastError` on the summary and
`Connected:true` + `System` on the snapshot, and asserts the row keeps nil gauges. It must
fail before the fix.

## 2. Prime placement — `internal/server/devices.go:612`

`PrimeStats()` sits *after* `syncPool()` and `syncAlertPool()`. `syncPool` starts the
overview dials; those return in 130 ms–2 s, i.e. inside the prime's own 1.5 s wait. So by
the time the frame is built the summary is routinely `Known` with a nil `System` — which is
precisely the window item 1's merge was added to paper over.

Priming **before** the two syncs would leave those routers not-Known at frame time, so the
existing else-branch builds the whole row from one source and no merge is needed.

Check before moving: whether the overview pool drops or keeps sessions across
suspend/resume, since a kept-and-Known session would reach the merge branch anyway.

**Verify:** the cold-open ordering test, plus item 10's new tests still passing.

## 3. The prime is TWO commands, not one — `internal/alertpool/prime.go:113`

`prime.go:32` claims "ONE read". `collect.NewSystem` leaves `healthAt` zero, and
`System.Tick` (`internal/collect/system.go:376-386`) computes
`doHealth := time.Since(s.healthAt) >= systemHealthEvery` — true on a fresh collector — so
the tick issues `/system/health/print` **then** `/system/resource/print`. Two separately
roslimit-gated commands per collector-less router.

The health row's only consumer is `TempC` (`system.go:209`), and nothing outside
`internal/collect` reads it: not `BuildRow`, not the frontend.

Same comment block is wrong about the static read: `Arch` comes from the resource print
(`system.go:186-189`), so only `Serial` and `LicenseLevel` stay nil.

**Fix:** have the throwaway collector skip the health menu, and correct the comment.

**Verify:** an assertion that the prime issues exactly one command. It must fail before the
fix (today it sees two).

## 4. The prime is unbounded — `internal/alertpool/prime.go:68`

Three things compound:

- neither command carries `Cmd.Timeout`, and `routeros.Client.Do` uses `context.Background`
  when it is zero (`internal/routeros/client.go:280-287`);
- `reader.Do` holds a `roslimit` slot for the whole round trip
  (`internal/alertpool/collectors.go:41-43`);
- `primeStats` has no in-flight or already-primed guard, and a router switch is three
  `devicesFocus` calls.

So each focus on a connected-but-unanswering router parks another goroutine holding another
slot. It self-heals in ~17 s (overview dial timeout plus a tick), but a `Timeout` on the
commands removes it outright.

**Fix:** stamp a deadline on the prime's commands, and skip a session that is already
priming.

## 5. False comment — `internal/alertpool/prime.go:16`

"its result still lands, and the next snapshot two seconds later carries it" is not true in
the real flow: the tick runs `syncAlertPool` before `sendRoutersStats`
(`devices.go:668-670`), `alertPoolExclusions` drops the router as soon as the overview
summary is `Known` (`alertpool_wire.go:155-161`), and `Sync` deletes the session before
`Snapshots` could read it. A late prime lands in a session nobody reads.

## 6. `PrimeStats` blocks the WebSocket reader — `internal/server/devices.go:612`

Dispatch is synchronous, and `devicesFocus` is reachable from `pageFocus` and from
`selectRouter` via `rejoinPage`, so one focus can stall every inbound frame for the whole
deadline and a router switch can do it three times. `startDevicesTick` is also started
after the wait, pushing the second frame out.

Bounded by items 2 and 4; decide whether that is enough or whether the prime should hand
back a second frame instead of holding the first.

## 7. `-race` failure on `historyOn` — `internal/routers/pool.go:717`

**Pre-existing, introduced in `d8fe2f8`, not by this commit** — but the suite this commit
extends is what goes red. `applyReporting` writes `s.historyOn` under `s.mu`
(`pool.go:373-376`); `startCollectors` reads it unlocked (`pool.go:717`).

**Fix:** read it under the lock.

**Verify:** `go test -race ./internal/routers/ ./internal/server/` goes green. Needs a cgo
image, so `golang:1.25` (Debian), not Alpine.

## 8. Stale "primed N/M" — `internal/alertpool/prime.go:94`

The tally counts every session whose `primedSystem()` is non-nil, and `s.primed` is never
cleared, so a value set by an earlier focus counts as this call's success. A router that has
stopped answering still logs "primed 3/3", pointing away from the hang the line exists to
find. Sessions with a nil conn are lumped in with real failures.

## 9. Weak ordering pin — `internal/verify/prime_test.go:52`

Three problems: it scans the function body with comments included, so a comment containing
`PrimeStats()` satisfies it; `prime >= 0 && send >= 0` makes the order check vacuous if
`sendRoutersStats` is renamed (`Index` returns -1); and its stated premise that
`devicesFocus` cannot be driven directly is false — `devices_test.go` already does it.

**Fix:** pin the behaviour on the existing `devicesConn` harness instead of the text.

## 10. Nothing can fail on items 1 or 3 — `internal/server/devices_test.go:862`

`TestTheAlertPoolFillsAGapInAnAnsweredSummary` sets `Connected:true` on both sides, so the
mixed-connection frame is untested; the IfStatus-onto-answered-System branch is unexercised;
and `prime_test.go` asserts nothing about which commands the prime issued.

## 11. Collector-less after focus is never primed — `internal/server/server.go:427`

`PrimeStats` runs only from `devicesFocus`. The one flow that produces a connected,
collector-less session *while Devices is open* is an interactive session idling out:
`SetOnIdle` calls `syncAlertPool` only, and the overview pool learns of the router at the
next tick. That card shows a green badge over blank gauges for a frame or two. Same
magnitude as the defect this commit fixed, and bounded.

## 12. Contradicted comments — `internal/alertpool/pool.go:299`

- `pool.go:287-288` "for a router the overview pool has not reached" and `:299-300`
  "ALREADY BEING COLLECTED … costs no extra router channel" are both false for the
  `primedSystem()` branch at `:358`.
- `pool.go:302-308` and `prime.go:24-26` describe the scope as status-only, but the
  `s.system == nil` filter also primes **history-only** sessions
  (`collectors.go:122-127` build ping and traffic, no system) — a real bonus fix that is
  nowhere written down, and therefore one a later tightening would silently undo.

## 13. Test fake panics on teardown — `internal/alertpool/prime_test.go:132`

The hand-built `poolSession` sets `system` and leaves `ping`, `ifStatus`, `vpn`, `netwatch`
and `routing` nil. `stopCollectors` (`collectors.go:196-207`) is unguarded once
`system != nil`, so any teardown of that session panics on a nil `Stop`. It is defused only
by never calling `p.Close`.

## 14. `primedMu` and the two-pass count — `internal/alertpool/pool.go:131`

`primed` is touched only by `primeSystem`/`primedSystem`, neither of which runs while
`s.mu` is held, so the second mutex buys nothing. The WaitGroup + done channel + `time.After`
+ second counting pass can be one buffered result channel counted as results arrive, which
also fixes item 8 and leaves no timer pending after an early return.

**Do not** go further and give status-only sessions a never-started `System` collector:
`startCollectors`/`stopCollectors` key on `s.system == nil` and would call `Start`/`Stop` on
nil collectors.

## 15. Doc drift — `CLAUDE.md:451`, `CONTRIBUTING.md:50`

The diff bumped the verify-test count by hand in two places, but
`TestDocumentedClaimsAreTrue` matches only the table row — the prose copy is unpinned.
`CONTRIBUTING.md` still says 23 Go tests and 15 test files against a current 34 and 22.
