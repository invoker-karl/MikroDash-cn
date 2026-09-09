package server

// The two fleet syncs resolve the default interface and declare what history
// keeps. Both halves were the bug: the syncs took `r.DefaultIf` RAW, so a
// router with none gave the traffic collector an empty interface list — and
// `syncStream` opens nothing for an empty list, so the pools that run when
// nobody is watching recorded no traffic at all. Issue #126.

import (
	"os"
	"path/filepath"
	"testing"

	"mikrodash/internal/historywire"
	"mikrodash/internal/routeros"
	"mikrodash/internal/routers"
)

// Neither router names a default interface on r1; r2 names its own.
const blankIfFixture = `[
  {"id":"r1","label":"One","host":"198.51.100.1","port":8728,"username":"u","password":"",
   "defaultIf":""},
  {"id":"r2","label":"Two","host":"198.51.100.2","port":8728,"username":"u","password":"",
   "defaultIf":"sfp1"}
]`

func recServer(t *testing.T) *Server {
	t.Helper()
	s, _, dir := routersServer(t, &Session{AuthMode: "none", Username: "admin"},
		`{"defaultIf":"ether5"}`)
	if err := os.WriteFile(filepath.Join(dir, "routers.json"),
		[]byte(blankIfFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	s.historyWire = historywire.New(true, nil)
	return s
}

// TestTheGlobalDefaultInterfaceIsRead — the middle rung of the precedence, and
// the one neither pool consulted.
func TestTheGlobalDefaultInterfaceIsRead(t *testing.T) {
	s := recServer(t)
	if got := s.globalDefaultIf(); got != "ether5" {
		t.Fatalf("globalDefaultIf = %q, want the install setting", got)
	}
	if got := routers.DefaultIfFor("", s.globalDefaultIf()); got != "ether5" {
		t.Errorf("a blank defaultIf resolved to %q", got)
	}
}

// TestTheFleetHoldSyncDeclaresWhatToRecord. These are the holds that keep
// routers nobody is watching connected, so their declaration is what makes a
// series continuous rather than following a browser tab.
//
// `holdFleet` is set by hand because `New` derives it from `-no-pool` and this
// test has no server options; without it `syncFleetHolds` returns at its first
// line and every assertion below passes for the wrong reason.
func TestTheFleetHoldSyncDeclaresWhatToRecord(t *testing.T) {
	s := recServer(t)
	s.holdFleet = true

	s.syncFleetHolds()

	if !s.historyWire.Records("r1", "ether5") {
		t.Error("r1 does not record the install-wide default interface, so a " +
			"router with no default of its own records nothing in the background")
	}
	if s.historyWire.Records("r1", "ether3") {
		t.Error("r1 still records an interface only a viewer would have added — " +
			"the browser is deciding what gets written")
	}
	if !s.historyWire.Records("r2", "sfp1") {
		t.Error("r2 does not record its own default interface")
	}
}

// TestTheOverviewPoolSyncDeclaresItToo — either pool may hold a given router,
// so a declaration made by only one of them leaves gaps on handover.
func TestTheOverviewPoolSyncDeclaresItToo(t *testing.T) {
	s := recServer(t)
	s.pool = routers.NewPool(
		func(routeros.Config) (routers.Conn, error) { return nil, os.ErrClosed },
		0, nil, nil)
	t.Cleanup(s.pool.Close)

	s.syncPool()

	if !s.historyWire.Records("r1", "ether5") {
		t.Error("the overview pool's sync declared nothing for r1")
	}
	if s.historyWire.Records("r2", "ether5") {
		t.Error("r2 records r1's interface; the declaration is not per router")
	}
}

// ── THE OUTAGE DEBOUNCE MOVED, AND SO DID ITS TESTS ────────────────────────
//
// Three tests stood here: that each sync cached a router's debounce, that a
// brief drop was not recorded, and that a router asking for zero recorded at
// once. All three drove `alertPoolStatus`, the hook `internal/alertpool` called
// on a connect or a drop, and that package and that hook are gone.
//
// The property is unchanged and is asserted where it now lives:
// `internal/session/connthresh_test.go` pins that the session builds
// `connThreshMs` from the router's own record and passes it to every
// `history.Connected`/`.Disconnected` call, and `internal/historywire/conn_test.go`
// already held the debounce behaviour itself — a drop and return inside the
// window writing nothing, a zero threshold writing at once.
//
// Recorded rather than silently dropped: a check removed without a reason reads
// exactly like one that never existed.
