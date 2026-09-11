package hub

// Declared events: the one place an event name and its payload type are bound.
//
// ── WHY THE EVENT CARRIES ITS PAYLOAD TYPE ─────────────────────────────────
//
// The browser's view of a payload used to be a TypeScript interface somebody
// kept in step by reading the Go. Nothing broke when the two drifted; the page
// rendered a field that was always undefined. cmd/tsgen generates those types
// from the Go structs, but generation only helps if the Go side SAYS which
// struct each event carries, and `Send(c, "routing:update", payload)` with a
// string and an `any` states neither.
//
// So an event is a value:
//
//	var EvRoutingUpdate = hub.Declare[RoutingPayload]("routing:update")
//
// Every way of putting a frame on the wire takes one, and the typed methods
// take a payload of exactly T — the compiler, not a test, proves each send
// matches its declaration. cmd/tsgen reads the same declarations to write the
// browser's event map, so both sides work from one list.
//
// ── THE ONE UNTYPED SEAM ───────────────────────────────────────────────────
//
// Collectors do not own the hub. They send through a closure the session
// provides, because that closure also runs the alert evaluator, the history
// recorder and the diagnostics counter on every payload. A closure cannot be
// generic, so the payload crosses it as `any`. It still cannot carry a wrong
// type: a Relay is opaque, so the only way to invoke one is Event[T].Emit,
// which takes a T; and Hub.Forward, where the closure hands the frame back,
// checks the payload against the event before anything is sent.

import (
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// Event is a declared WebSocket event whose payload is a T.
type Event[T any] struct{ name string }

// Named is a declared event of any payload type. The unexported method means
// only an Event can satisfy it, so an event name reaches the wire only by
// having been declared.
type Named interface {
	Name() string
	accepts(payload any) bool
}

// Name is the event's wire name.
func (e Event[T]) Name() string { return e.name }

func (e Event[T]) accepts(payload any) bool {
	_, ok := payload.(T)
	return ok
}

var (
	registryMu sync.Mutex
	registry   = map[string]reflect.Type{}
)

// Declare binds an event name to its payload type. It is meant for
// package-level vars.
//
// A NAME DECLARED TWICE PANICS. Two declarations of one event could disagree
// about its type, which is the drift this file exists to prevent, and an event
// sent from two packages is declared once and imported by the other.
func Declare[T any](name string) Event[T] {
	registryMu.Lock()
	defer registryMu.Unlock()
	t := reflect.TypeFor[T]()
	if prev, dup := registry[name]; dup {
		panic(fmt.Sprintf("hub: event %q declared twice (%v and %v)", name, prev, t))
	}
	registry[name] = t
	return Event[T]{name: name}
}

// Declared is one registered event.
type Declared struct {
	Name string
	Type reflect.Type
}

// Events lists every event declared in this binary, sorted by name.
//
// A binary sees the declarations of the packages it links. That is how a test
// finds every payload a package sends without anybody keeping a list.
func Events() []Declared {
	registryMu.Lock()
	defer registryMu.Unlock()
	out := make([]Declared, 0, len(registry))
	for name, t := range registry {
		out = append(out, Declared{Name: name, Type: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Send delivers the event to one client.
func (e Event[T]) Send(h *Hub, c *Client, p T) { h.send(c, e.name, p) }

// Broadcast delivers the event to everybody in a room.
func (e Event[T]) Broadcast(h *Hub, room string, p T) { h.broadcast(room, e.name, p) }

// BroadcastAll delivers the event to every connected client. See broadcastAll
// for why this is the exception rather than the rule.
func (e Event[T]) BroadcastAll(h *Hub, p T) { h.broadcastAll(e.name, p) }

// BroadcastExcept delivers the event to a room apart from one client.
func (e Event[T]) BroadcastExcept(h *Hub, room string, except *Client, p T) {
	h.broadcastExcept(room, except, e.name, p)
}

// BroadcastRooms delivers the event once to each client in any of the rooms.
func (e Event[T]) BroadcastRooms(h *Hub, rooms []string, p T) {
	h.broadcastRooms(rooms, e.name, p)
}

// Relay is how a producer that does not own the hub sends: through a closure
// that decides the rooms and may look at the payload on the way. The session
// builds one per router and hands it to every collector.
//
// OPAQUE ON PURPOSE. The closure takes `any`, so calling it directly would let
// a collector send a payload of the wrong type under a declared name. Only
// Event[T].Emit can invoke it, and that takes a T.
type Relay struct {
	fn func(room string, e Named, payload any)
}

// NewRelay wraps the closure that routes a producer's events.
func NewRelay(fn func(room string, e Named, payload any)) Relay { return Relay{fn: fn} }

// Emit sends the event through a Relay. A zero Relay drops it, which is what a
// collector built with no session to send to wants.
func (e Event[T]) Emit(r Relay, room string, p T) {
	if r.fn != nil {
		r.fn(room, e, p)
	}
}

// Forward is where a Relay's closure hands a frame back for delivery, once per
// client across the rooms.
//
// IT CHECKS THE PAYLOAD. A frame arriving through Event[T].Emit always passes,
// so a failure here means something built a Named and an `any` by another
// route. That is dropped and logged rather than sent, because the browser's
// type for the event would be wrong about it.
func (h *Hub) Forward(rooms []string, e Named, payload any) {
	if !e.accepts(payload) {
		log.Printf("[hub] %s: a %T is not its declared payload type; not sent", e.Name(), payload)
		return
	}
	h.broadcastRooms(rooms, e.Name(), payload)
}

// ── NO NULL ARRAYS ──────────────────────────────────────────────────────────

var jsonMarshaler = reflect.TypeFor[json.Marshaler]()

// NilSlices returns the path of every slice in v that would marshal as null.
//
// The browser's payload types say an array is always an array — cmd/tsgen
// emits `T[]`, never `T[] | null` — and a nil Go slice marshals to null. This
// is what holds the Go side to that: the payload tests build every declared
// payload and fail on any path it returns.
//
// A slice tagged `omitempty` is exempt, because it marshals to an ABSENT key,
// which the generated type already marks optional. A type with its own
// MarshalJSON is not walked: its fields say nothing about what it writes.
func NilSlices(v any) []string {
	var out []string
	walkNil(reflect.ValueOf(v), "$", false, &out)
	return out
}

func walkNil(v reflect.Value, path string, omitempty bool, out *[]string) {
	if !v.IsValid() {
		return
	}
	if v.Type().Implements(jsonMarshaler) ||
		(v.CanAddr() && v.Addr().Type().Implements(jsonMarshaler)) {
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walkNil(v.Elem(), path, false, out)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() && !f.Anonymous {
				continue
			}
			name, omit, skip := jsonField(f)
			if skip {
				continue
			}
			p := path
			if !f.Anonymous || name != "" {
				if name == "" {
					name = f.Name
				}
				p = path + "." + name
			}
			walkNil(v.Field(i), p, omit, out)
		}
	case reflect.Slice:
		if v.IsNil() {
			if !omitempty {
				*out = append(*out, path)
			}
			return
		}
		for i := 0; i < v.Len(); i++ {
			walkNil(v.Index(i), fmt.Sprintf("%s[%d]", path, i), false, out)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			walkNil(iter.Value(), fmt.Sprintf("%s.%v", path, iter.Key().Interface()), false, out)
		}
	}
}

// jsonField reads a field's json tag the way encoding/json does.
func jsonField(f reflect.StructField) (name string, omitempty, skip bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omitempty = true
		}
	}
	return parts[0], omitempty, false
}
