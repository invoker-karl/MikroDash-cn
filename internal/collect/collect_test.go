package collect

import "testing"

// ── PHASE 4.1: A CONSUMER DRIVEN BY A STUB ─────────────────────────────────
//
// This is the whole point of declaring the in-process edges as capabilities
// rather than as producer pointers. Before, naming a lease payload in a test
// meant constructing a `*DHCPLeases` — a reader, an emit function, a poll
// interval and a tick — so nobody did, and the join between a connection and a
// device name was only ever exercised end to end.

type stubLeases struct{ p *LeasesPayload }

func (s stubLeases) Last() *LeasesPayload { return s.p }

type stubNets struct{ p *LanPayload }

func (s stubNets) Last() *LanPayload { return s.p }

func TestAConsumerTakesItsEdgeFromAStub(t *testing.T) {
	leases := stubLeases{&LeasesPayload{Leases: []Lease{
		{IP: "10.0.0.5", Name: "kitchen-pi", MAC: "02:00:00:00:00:01"},
	}}}

	w := NewWireless(fakeReader{}, func(string, string, any) {}, leases, 30000)
	if w == nil {
		t.Fatal("NewWireless returned nil")
	}
	// The edge is now satisfiable by anything that can answer the question,
	// which is what makes the consumer testable at all.
	if got := w.leases.Last(); got == nil || len(got.Leases) != 1 {
		t.Fatalf("the stub was not reachable through the edge: %+v", got)
	}
	if got := w.leases.Last().Leases[0].Name; got != "kitchen-pi" {
		t.Errorf("name = %q, want kitchen-pi", got)
	}
}

// TestAnAbsentEdgeIsNil — a collector the operator has disabled is absent, and a
// consumer must cost exactly the field that edge fed rather than failing.
//
// A LITERAL nil IS AN UNTYPED NIL and the guards see it. A typed nil pointer
// passed as an interface would NOT be, which is the trap `collect.go` records:
// the interface carries the type, every `if x == nil` is false, and the first
// call dereferences a nil receiver in a consumer that has a nil check right
// there and looks correct.
func TestAnAbsentEdgeIsNil(t *testing.T) {
	w := NewWireless(fakeReader{}, func(string, string, any) {}, nil, 30000)
	if w.leases != nil {
		t.Error("a literal nil did not arrive as a nil interface; every nil guard in " +
			"this consumer would be false and the first call would panic")
	}

	c := NewConnections(fakeReader{}, func(string, string, any) {}, nil, nil, 3000)
	if c.leases != nil || c.nets != nil {
		t.Error("connections' absent edges are not nil")
	}
}
