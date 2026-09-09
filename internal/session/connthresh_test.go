package session

import (
	"regexp"
	"testing"

	"mikrodash/internal/historywire"
	"mikrodash/internal/store"
)

// ── THE OUTAGE DEBOUNCE REACHES THE RECORDER ───────────────────────────────
//
// Passing a hardcoded zero is not a small error: zero is its own branch meaning
// "record every close at once", and it turned a routine six-second reconnect
// into an outage in the Reports page. A mutation restoring that zero survived
// every other test in the package that used to hold this.
//
// ── RE-AIMED, NOT INHERITED ────────────────────────────────────────────────
//
// This lived in `internal/server/recorded_ifaces_test.go` and drove
// `alertPoolStatus`, the hook `internal/alertpool` called on a connect or a
// drop. That package is gone: every router nobody is watching is held as a
// SESSION now, so the session is the only writer of `connectivity_events` and
// its own `connThreshMs` is the only place the threshold can go wrong.
//
// The server's `connThresh` cache went with the hook. It was populated by both
// fleet syncs and read by nothing else, and a cache with no reader is exactly
// the folklore this repository's ledgers exist to prevent.

func ptr(n int) *int { return &n }

// TestUnsetIsNotZero is the distinction the whole debounce rests on.
func TestUnsetIsNotZero(t *testing.T) {
	if _, ok := connDownSecOf(&store.Router{}); ok {
		t.Error("a router with no setting reports one; the live 30s default is lost")
	}
	sec, ok := connDownSecOf(&store.Router{ConnDownThresholdSec: ptr(0)})
	if !ok || sec != 0 {
		t.Errorf("a deliberate zero read as (%d, %v), want (0, true)", sec, ok)
	}
	if got := historywire.ThresholdMs(connDownSecOf(&store.Router{})); got != 30_000 {
		t.Errorf("an unset debounce resolved to %dms, want the live 30s default", got)
	}
	if got := historywire.ThresholdMs(
		connDownSecOf(&store.Router{ConnDownThresholdSec: ptr(0)})); got != 0 {
		t.Errorf("a router asking for zero resolved to %dms; zero is a deliberate "+
			"setting, not an absence", got)
	}
	// A nil record is what `Acquire` holds before the store answers.
	if got := historywire.ThresholdMs(connDownSecOf(nil)); got != 30_000 {
		t.Errorf("a nil record resolved to %dms rather than the default", got)
	}
}

// TestTheSessionCarriesTheRoutersOwnThreshold pins the WIRING, which is the half
// a pure test of `connDownSecOf` cannot see: the three `history.Connected` /
// `.Disconnected` calls must pass `s.connThreshMs`, and nothing else.
//
// SOURCE-READ, because reaching those call sites needs a router. The mutation
// this kills is the one that was actually made: a literal 0 in the argument.
func TestTheSessionCarriesTheRoutersOwnThreshold(t *testing.T) {
	body := readSource(t, "session.go")

	if !regexp.MustCompile(`connThreshMs:\s+historywire\.ThresholdMs\(connDownSecOf\(rec\)\)`).
		MatchString(body) {
		t.Error("the session no longer builds connThreshMs from the router's own " +
			"record; every outage would be recorded against one threshold")
	}
	calls := regexp.MustCompile(`s\.history\.(Connected|Disconnected)\(([^)]*)\)`).
		FindAllStringSubmatch(body, -1)
	if len(calls) < 3 {
		t.Fatalf("found %d connectivity call(s); the session had three (one connect, "+
			"two teardown paths). A lost one is an outage nobody records.", len(calls))
	}
	for _, c := range calls {
		if !regexp.MustCompile(`\bs\.connThreshMs\b`).MatchString(c[2]) {
			t.Errorf("s.history.%s(%s) does not pass s.connThreshMs — a hardcoded "+
				"threshold turns a routine reconnect into a recorded outage", c[1], c[2])
		}
	}
}
