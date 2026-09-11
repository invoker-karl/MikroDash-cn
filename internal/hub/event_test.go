package hub

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// These declare their own events. Every name is prefixed `test:` because the
// registry is process-wide and a name declared twice panics by design.

// TestDeclaringANameTwicePanics — two declarations of one event could disagree
// about its payload type, which is the drift the registry exists to stop.
func TestDeclaringANameTwicePanics(t *testing.T) {
	Declare[int]("test:dup")
	defer func() {
		if recover() == nil {
			t.Error("a second declaration of test:dup did not panic")
		}
	}()
	Declare[string]("test:dup")
}

// TestEventsListsEveryDeclarationWithItsType — the payload tests find what to
// cover through this, so a declaration missing from it would go untested.
func TestEventsListsEveryDeclarationWithItsType(t *testing.T) {
	Declare[map[string]any]("test:listed")
	var found bool
	prev := ""
	for _, d := range Events() {
		if d.Name < prev {
			t.Fatalf("Events is not sorted: %q after %q", d.Name, prev)
		}
		prev = d.Name
		if d.Name == "test:listed" {
			found = true
			if d.Type != reflect.TypeFor[map[string]any]() {
				t.Errorf("test:listed has type %v, want map[string]any", d.Type)
			}
		}
	}
	if !found {
		t.Error("a declared event is missing from Events()")
	}
}

// TestEmitReachesTheRelayWithItsEvent — the closure must see the declared
// event, not just a name, because it hands that event to Forward.
func TestEmitReachesTheRelayWithItsEvent(t *testing.T) {
	ev := Declare[int]("test:relayed")
	var gotRoom, gotName string
	var gotPayload any
	r := NewRelay(func(room string, e Named, p any) {
		gotRoom, gotName, gotPayload = room, e.Name(), p
	})
	ev.Emit(r, "page-x", 7)
	if gotRoom != "page-x" || gotName != "test:relayed" || gotPayload != 7 {
		t.Errorf("relay saw (%q, %q, %v), want (page-x, test:relayed, 7)", gotRoom, gotName, gotPayload)
	}
	// A zero Relay drops rather than panicking: collectors are built without a
	// session in tests and in the Devices pool's probe.
	ev.Emit(Relay{}, "page-x", 7)
}

// TestForwardRefusesAPayloadOfTheWrongType — the relay seam carries `any`, so
// this is the last point a wrong type could be stopped before the browser,
// whose generated type for the event would be wrong about it.
func TestForwardRefusesAPayloadOfTheWrongType(t *testing.T) {
	ev := Declare[int]("test:forward")
	h := New()
	c := NewClient("ws-1", 4)
	h.Add(c)
	h.Join(c, "room")

	h.Forward([]string{"room"}, ev, "not an int")
	if len(c.Send) != 0 {
		t.Fatal("a string was delivered under an event declared to carry an int")
	}
	h.Forward([]string{"room"}, ev, 3)
	if len(c.Send) != 1 {
		t.Fatalf("a correctly typed payload was not delivered (queue %d)", len(c.Send))
	}
	var env struct {
		Event string `json:"event"`
		Data  int    `json:"data"`
	}
	if err := json.Unmarshal(<-c.Send, &env); err != nil || env.Event != "test:forward" || env.Data != 3 {
		t.Errorf("frame = %+v (err %v), want test:forward carrying 3", env, err)
	}
}

type rawWriter struct{ Items []int }

func (rawWriter) MarshalJSON() ([]byte, error) { return []byte(`"raw"`), nil }

type inner struct {
	Tags []string `json:"tags"`
}

type embedded struct {
	Deep []int `json:"deep"`
}

type sample struct {
	embedded
	Filled  []int            `json:"filled"`
	Nil     []int            `json:"nil"`
	Omitted []int            `json:"omitted,omitempty"`
	Hidden  []int            `json:"-"`
	Rows    []inner          `json:"rows"`
	ByKey   map[string]inner `json:"byKey"`
	Ptr     *inner           `json:"ptr"`
	Custom  rawWriter        `json:"custom"`
	private []int
}

// TestNilSlicesFindsEveryArrayThatWouldBeNull — each exemption is asserted as
// well as each finding, because a checker that exempts too much reports a
// clean payload that sends null.
func TestNilSlicesFindsEveryArrayThatWouldBeNull(t *testing.T) {
	v := sample{
		Filled: []int{1},
		Rows:   []inner{{Tags: []string{"a"}}, {}},
		ByKey:  map[string]inner{"k": {}},
		Ptr:    &inner{},
	}
	got := strings.Join(NilSlices(v), " ")
	for _, want := range []string{"$.deep", "$.nil", "$.rows[1].tags", "$.byKey.k.tags", "$.ptr.tags"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s would marshal as null and was not reported; got %q", want, got)
		}
	}
	for _, not := range []string{"$.filled", "$.omitted", "$.Hidden", "$.custom", "private", "$.rows[0]"} {
		if strings.Contains(got, not) {
			t.Errorf("%s was reported, and it cannot marshal as null; got %q", not, got)
		}
	}

	// And the report agrees with encoding/json, which is the only authority on
	// what reaches the browser.
	b, _ := json.Marshal(v)
	for _, key := range []string{`"nil":null`, `"deep":null`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("expected %s in the marshalled form, got %s", key, b)
		}
	}
}
