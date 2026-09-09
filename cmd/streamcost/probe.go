package main

// B.4's prerequisite: DOES THIS MENU ACCEPT `=interval=N` OVER THE BINARY API?
//
// ── WHY THIS IS NOT ANSWERABLE FROM THE DOCUMENTATION ──────────────────────
//
// The scripting manual lists `interval` among the common print parameters --
// "continuously print output in a selected time interval" -- and that settles the
// CLI. It does not settle the binary API, which is what this app speaks, and the
// REST API page says the opposite for ITS transport: "API does not support
// continuous commands such as monitor. Use the monitor once parameter."
//
// This app already streams `/interface/monitor-traffic` and `/tool/ping` over the
// binary API, so continuous commands plainly work there. What is unproven is a
// PLAIN `/print` with an interval, which is what nineteen of the twenty
// dual-capable collectors would use.
//
// Assuming and being wrong is a bad failure shape: it would surface as a trap at
// the moment a router is switched to Stream, per collector, on a page that was
// rendering. So B.4 checks first, and this is the check.
//
// ── WHAT IT REPORTS ────────────────────────────────────────────────────────
//
//	OPENED + rows     the menu streams; a collector may be enabled for it
//	OPENED, no rows   the channel was accepted and nothing came down it. That is
//	                  WORSE than a refusal and is why the row count is here: a
//	                  collector on such a menu would hold a channel, deliver
//	                  nothing, and look exactly like a working stream.
//	REFUSED           the router said no. `scheduled` falls back to polling, so
//	                  this is safe -- it just means the menu stays polled.
//
// READ-ONLY. It opens channels and closes them, and writes nothing.

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"mikrodash/internal/routeros"
)

type probeResult struct {
	menu  string
	err   error
	rows  int64
	first time.Duration
	// base is what a PLAIN read of the menu returns, and it is what makes a
	// silent channel interpretable.
	//
	// ── THE FIRST VERSION OF THIS PROBE COULD NOT TELL TWO CASES APART ──────
	//
	// `/ppp/active/print` and `/routing/bgp/session/print` came back with zero
	// rows and were reported as "accepted but silent", as though the router had
	// ignored the interval. Both menus are simply EMPTY on the router that was
	// probed -- no active PPP sessions, no BGP sessions -- so a working stream
	// would also deliver nothing.
	//
	// Recording that as "does not support interval" would have put a false fact
	// in the plan and kept two collectors polled for a reason that was never
	// measured. A baseline read is one command and removes the ambiguity.
	base int
}

// probeMenus opens each menu with an interval, holds it, and counts what arrives.
//
// ONE AT A TIME, not concurrently. B.0b established that concurrency is not the
// constraint; running these in sequence keeps a refusal attributable to its own
// menu rather than to the load of nineteen others.
func probeMenus(c *routeros.Client, menus []string, hold int) []probeResult {
	out := make([]probeResult, 0, len(menus))
	for _, menu := range menus {
		// THE BASELINE FIRST. One plain read, so a silent channel can be told
		// apart from an empty table.
		baseRows, baseErr := c.Do(routeros.Cmd{Path: menu})
		if baseErr != nil {
			out = append(out, probeResult{menu: menu, err: baseErr})
			continue
		}

		var rows atomic.Int64
		var firstAt atomic.Int64
		start := time.Now()

		stop, err := c.Stream(routeros.Cmd{Path: menu, Args: []string{"=interval=1"}},
			func(routeros.Reply) {
				rows.Add(1)
				firstAt.CompareAndSwap(0, int64(time.Since(start)))
			})
		if err != nil {
			out = append(out, probeResult{menu: menu, err: err})
			continue
		}
		time.Sleep(time.Duration(hold) * time.Second)
		stop()
		out = append(out, probeResult{
			menu:  menu,
			rows:  rows.Load(),
			first: time.Duration(firstAt.Load()),
			base:  len(baseRows),
		})
	}
	return out
}

func reportProbe(res []probeResult, hold int) {
	sort.Slice(res, func(i, j int) bool { return res[i].menu < res[j].menu })

	var ok, silent, empty, refused []string
	fmt.Printf("  %-46s %-7s %-8s %-9s %s\n", "menu", "rows", "in table", "first", "verdict")
	fmt.Printf("  %-46s %-7s %-8s %-9s %s\n", strings.Repeat("-", 46), "----", "--------", "-----", "-------")
	for _, r := range res {
		switch {
		case r.err != nil:
			fmt.Printf("  %-46s %-7s %-8s %-9s REFUSED: %s\n", r.menu, "-", "-", "-",
				firstLine(r.err.Error()))
			refused = append(refused, r.menu)
		case r.rows == 0 && r.base == 0:
			// NOT A VERDICT. The table is empty, so a working stream and a
			// broken one look the same. Re-probe on a router that has rows.
			fmt.Printf("  %-46s %-7d %-8d %-9s UNTESTED (table empty here)\n", r.menu, 0, 0, "-")
			empty = append(empty, r.menu)
		case r.rows == 0:
			fmt.Printf("  %-46s %-7d %-8d %-9s ACCEPTED BUT SILENT\n", r.menu, 0, r.base, "-")
			silent = append(silent, r.menu)
		default:
			fmt.Printf("  %-46s %-7d %-8d %-9s ok\n", r.menu, r.rows, r.base,
				r.first.Round(time.Millisecond).String())
			ok = append(ok, r.menu)
		}
	}
	fmt.Println()
	fmt.Printf("  %d of %d menus streamed over %ds.\n", len(ok), len(res), hold)
	if len(silent) > 0 {
		// NAMED SEPARATELY BECAUSE IT IS THE DANGEROUS OUTCOME. A refusal falls
		// back to polling and everything keeps working. A channel that opens and
		// delivers nothing looks identical to a working stream from inside the
		// app: the collector holds it, the page goes stale, and nothing errors.
		fmt.Printf("  ACCEPTED BUT SILENT (do NOT enable these; a channel that delivers\n" +
			"  nothing is indistinguishable from a working one inside the app):\n")
		for _, m := range silent {
			fmt.Printf("    %s\n", m)
		}
	}
	if len(empty) > 0 {
		fmt.Printf("  UNTESTED — the table is EMPTY on this router, so a working stream\n" +
			"  and a broken one deliver the same nothing. Re-probe where there are rows:\n")
		for _, m := range empty {
			fmt.Printf("    %s\n", m)
		}
	}
	if len(refused) > 0 {
		fmt.Printf("  REFUSED (safe — `scheduled` falls back to polling):\n")
		for _, m := range refused {
			fmt.Printf("    %s\n", m)
		}
	}
	fmt.Println()
}
