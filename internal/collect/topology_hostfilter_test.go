package collect

import (
	"testing"

	"mikrodash/internal/routeros"
)

// ── THE GOLDEN CANNOT COVER THIS, SO IT IS COVERED HERE ─────────────────────
//
// topology used to ask the router for non-local bridge hosts with `?local=false`.
// A query argument makes a command uncacheable — roscache keys by menu, and
// `bridges` asks the same menu for the whole table — so on 2026-09-08 the
// operator traded the router-side filter for a shared read, and the filter moved
// into readHosts.
//
// The fixture was captured THROUGH the old query, so every recorded row is
// already non-local and replaying it exercises nothing: the golden stayed green
// because it has no local host to drop. That is precisely the shape of gap
// CLAUDE.md warns about, so the filter gets its own test with rows the router
// would now actually send.

type hostFilterReader struct{ rows []routeros.Reply }

func (h *hostFilterReader) Connected() bool { return true }
func (h *hostFilterReader) Do(cmd routeros.Cmd) ([]routeros.Reply, error) {
	if cmd.Path == "/interface/bridge/host/print" {
		return h.rows, nil
	}
	return nil, nil
}

func TestTopologyDropsLocalBridgeHosts(t *testing.T) {
	r := &hostFilterReader{rows: []routeros.Reply{
		{"mac-address": "02:00:00:00:00:01", "on-interface": "ether2", "bridge": "bridge", "vid": "10", "local": "false"},
		{"mac-address": "02:00:00:00:00:02", "on-interface": "bridge", "bridge": "bridge", "vid": "10", "local": "true"},
		// No `local` at all. Absence is NOT local: the old query would have
		// returned such a row, and treating it as local would empty the map on
		// any RouterOS build that stops reporting the field.
		{"mac-address": "02:00:00:00:00:03", "on-interface": "ether3", "bridge": "bridge", "vid": "20"},
	}}
	c := NewTopology(r, func(string, string, any) {}, nil, "r1", "lab", 30000)

	hosts, vlans := c.readHosts()

	if len(hosts) != 2 {
		t.Fatalf("kept %d hosts, want 2: %+v", len(hosts), hosts)
	}
	for _, h := range hosts {
		if h.MAC == "02:00:00:00:00:02" {
			t.Errorf("the local host survived the filter: %+v", hosts)
		}
	}
	if len(vlans["02:00:00:00:00:02"]) != 0 {
		t.Errorf("a local host contributed a VLAN: %v", vlans)
	}
	if len(vlans["02:00:00:00:00:01"]) != 1 || vlans["02:00:00:00:00:01"][0] != 10 {
		t.Errorf("non-local VLANs lost: %v", vlans)
	}
}

// TestTopologyHostReadCarriesLocal pins the half that makes the filter work at
// all. topology's proplist is what the router answers when topology reads first,
// so dropping `local` from it would leave the filter comparing empty strings and
// silently keeping every local host.
func TestTopologyHostReadCarriesLocal(t *testing.T) {
	fields, ok := cacheableFields(topoHostsCmd)
	if !ok {
		t.Fatal("topoHostsCmd is not cacheable — a query argument came back and the shared read is gone")
	}
	found := false
	for _, f := range fields {
		if f == "local" {
			found = true
		}
	}
	if !found {
		t.Errorf("topoHostsCmd does not ask for `local`, so readHosts cannot filter on it: %v", fields)
	}
}
