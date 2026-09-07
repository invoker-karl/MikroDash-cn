package session

// The check `dormancy_payload.go` has claimed for months and which did not
// exist.
//
// Its header says "`TestEveryEligibleCollectorsPayloadHasItsEmptyKey` pins that
// every key the registry names actually resolves here". Nothing of that name was
// ever defined — a comment describing a gate is not a gate, and this one had
// been read as coverage.
//
// ── WHAT GOES WRONG WITHOUT IT ──────────────────────────────────────────────
//
// `emptyKey` lives in `collection_tables.json` and names json tags on a payload
// STRUCT in another package. Nothing connects the two at compile time. Misspell
// one, rename a field, or add a registry key with no field behind it, and
// `fieldByJSONTag` returns the zero Value, `payloadLookup` reports "not a list",
// and `PayloadEmptyBy` SKIPS it. Skipping every listed key makes `readable`
// false, so the collector can never be judged empty and never sleeps — silently,
// forever, with a green suite and no log line.
//
// That is not hypothetical bookkeeping: it is the failure this file's own
// comment says is covered.
//
// ── WHY REFLECTION OVER THE SESSION, RATHER THAN A TABLE ────────────────────
//
// A hand-written key→type table would drift from the registry exactly the way
// eighteen closures would, which is the argument `dormancy_payload.go` already
// makes for reading json tags instead of restating fields. So this walks the
// registry, finds each collector's field on `Session` by its `sessionProp`, and
// takes the payload type off its `Last()` method. Add a collector and it is
// covered without anyone remembering to add it.

import (
	"reflect"
	"strings"
	"testing"

	"mikrodash/internal/collect"
	"mikrodash/internal/collection"
)

// fieldTypeByJSONTag is `fieldByJSONTag`'s type-level twin: the same tag rule
// (everything before the comma), asked of a Type rather than a Value.
func fieldTypeByJSONTag(t reflect.Type, name string) (reflect.StructField, bool) {
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" {
			continue
		}
		if before, _, _ := strings.Cut(tag, ","); before == name {
			return t.Field(i), true
		}
	}
	return reflect.StructField{}, false
}

// sessionField finds a collector's field on Session, case-insensitively.
//
// ── WHY NOT AN EXACT MATCH ──────────────────────────────────────────────────
//
// `sessionProp` is the LIVE JAVASCRIPT property name, not this port's Go field
// name, and for one collector they differ: the registry says `rosusers` where
// Session says `rosUsers`. That is a rename this port made, not a defect — the
// runtime path reaches it directly (`s.rosUsers.Last()` in dormancy_targets.go),
// so the collector is correctly wired. Only a test naive enough to match
// exactly would call it broken, and this one did on its first run.
//
// Ambiguity is still a failure: two fields differing only in case would make the
// choice arbitrary, and silently picking one is how a test starts checking the
// wrong type.
func sessionField(t *testing.T, sess reflect.Type, prop string) (reflect.StructField, bool) {
	t.Helper()
	var found reflect.StructField
	n := 0
	for i := 0; i < sess.NumField(); i++ {
		if strings.EqualFold(sess.Field(i).Name, prop) {
			found, n = sess.Field(i), n+1
		}
	}
	if n > 1 {
		t.Errorf("sessionProp %q matches %d Session fields case-insensitively; "+
			"the mapping is ambiguous", prop, n)
		return reflect.StructField{}, false
	}
	return found, n == 1
}

func TestEveryEligibleCollectorsPayloadHasItsEmptyKey(t *testing.T) {
	sess := reflect.TypeOf(Session{})
	eligible := collection.DormancyEligible()
	if len(eligible) == 0 {
		t.Fatal("no dormancy-eligible collectors — the registry did not load")
	}

	checked := 0
	for _, c := range eligible {
		if c.SessionProp == "" {
			t.Errorf("%s: eligible for dormancy but declares no sessionProp, so "+
				"nothing can reach its payload", c.Key)
			continue
		}
		field, ok := sessionField(t, sess, c.SessionProp)
		if !ok {
			t.Errorf("%s: sessionProp %q names no field on Session",
				c.Key, c.SessionProp)
			continue
		}
		last, ok := field.Type.MethodByName("Last")
		if !ok || last.Type.NumOut() != 1 {
			t.Errorf("%s: %s has no single-result Last() to take a payload type from",
				c.Key, field.Type)
			continue
		}
		pt := last.Type.Out(0)
		for pt.Kind() == reflect.Ptr {
			pt = pt.Elem()
		}
		if pt.Kind() != reflect.Struct {
			t.Errorf("%s: Last() returns %s, not a struct", c.Key, pt)
			continue
		}

		for _, key := range c.EmptyKey {
			f, ok := fieldTypeByJSONTag(pt, key)
			if !ok {
				t.Errorf("%s: emptyKey %q resolves to no json tag on %s — "+
					"PayloadEmptyBy will skip it, and a collector whose every key "+
					"is skipped can never be judged empty",
					c.Key, key, pt)
				continue
			}
			if f.Type.Kind() != reflect.Slice {
				t.Errorf("%s: emptyKey %q is %s.%s of kind %s, not a slice — "+
					"payloadLookup reports anything but a slice as 'not a list'",
					c.Key, key, pt, f.Name, f.Type.Kind())
			}
			checked++
		}
	}

	// If the registry ever stops loading, or `DormancyEligible` starts returning
	// rows with no keys, every loop above is a no-op and this test passes while
	// measuring nothing.
	if checked == 0 {
		t.Fatal("resolved no emptyKey at all — this test measured nothing")
	}
	t.Logf("resolved %d emptyKey entries across %d eligible collectors",
		checked, len(eligible))
}

// The three-row table the IPv6 firewall work turns on, asserted directly.
//
// Everything else about that change is structure — keys resolve, fields exist.
// This is the BEHAVIOUR, and it is the piece a green suite elsewhere says
// nothing about: whether a dual-stack router is judged the way we claimed.
//
// The whole design rests on one property of `PayloadEmptyBy`: a key that is not
// a list is SKIPPED, and a nil Go slice is not a list. So the four IPv6 tables
// can sit in the payload schema permanently while being invisible to dormancy
// on every router where nobody has asked for IPv6. Get it backwards — emit `[]`
// instead of nil when v6 is uncollected — and an IPv4-only router with no rules
// becomes un-sleepable, holding a session nobody is watching.
func TestFirewallEmptinessIgnoresUncollectedIPv6(t *testing.T) {
	var fw *collection.Collector
	for _, c := range collection.DormancyEligible() {
		if c.Key == "firewall" {
			cc := c
			fw = &cc
		}
	}
	if fw == nil {
		t.Fatal("firewall is not dormancy-eligible — the registry did not load")
	}

	rule := []collect.FirewallRule{{ID: "*1", Chain: "input", Action: "drop"}}
	empty := []collect.FirewallRule{}

	cases := []struct {
		name    string
		payload *collect.FirewallPayload
		empty   bool
		why     string
	}{
		{
			"IPv4-only router, IPv6 never asked for",
			&collect.FirewallPayload{Filter: rule, Nat: empty, Mangle: empty, Raw: empty},
			false,
			"v4 rules exist; the nil v6 tables must be skipped, not counted as empty",
		},
		{
			"nothing anywhere, IPv6 never asked for",
			&collect.FirewallPayload{Filter: empty, Nat: empty, Mangle: empty, Raw: empty},
			true,
			"judged on v4 alone, exactly as before this change",
		},
		{
			"dual-stack, IPv6 showing, rules only in IPv6",
			&collect.FirewallPayload{
				Filter: empty, Nat: empty, Mangle: empty, Raw: empty,
				Filter6: rule, Nat6: empty, Mangle6: empty, Raw6: empty,
			},
			false,
			"THE CASE THAT MOTIVATED ADDING THE KEYS: without them this router " +
				"is slept while the operator is looking at its IPv6 rules",
		},
		{
			"IPv6 showing, genuinely nothing in either family",
			&collect.FirewallPayload{
				Filter: empty, Nat: empty, Mangle: empty, Raw: empty,
				Filter6: empty, Nat6: empty, Mangle6: empty, Raw6: empty,
			},
			true,
			"there is nothing to report, so sleeping is right",
		},
		{
			"IPv6 never asked for, but v4 has rules and v6 would have too",
			&collect.FirewallPayload{Filter: rule, Nat: empty, Mangle: empty, Raw: empty},
			false,
			"same as row one; uncollected v6 can never change a verdict either way",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collection.PayloadEmptyBy(payloadLookup(tc.payload), fw.EmptyKey)
			if got != tc.empty {
				t.Errorf("empty=%t, want %t — %s", got, tc.empty, tc.why)
			}
		})
	}

	// The inverse of row one, stated as its own assertion because it is the
	// mistake with the worst consequence and the least visibility: emitting `[]`
	// for the v6 tables when nobody asked would make an IPv4-only router with no
	// rules read as... still empty here, but only because every key is empty.
	// The damage shows on a router that HAS v4 rules: unchanged. So the honest
	// check is the one above — nil must be SKIPPED, and this proves the skip by
	// showing a nil-v6 payload and an empty-v6 payload disagree.
	withNil := &collect.FirewallPayload{Filter: empty, Nat: empty, Mangle: empty, Raw: empty}
	withEmpty := &collect.FirewallPayload{
		Filter: empty, Nat: empty, Mangle: empty, Raw: empty,
		Filter6: rule, Nat6: empty, Mangle6: empty, Raw6: empty,
	}
	if collection.PayloadEmptyBy(payloadLookup(withNil), fw.EmptyKey) ==
		collection.PayloadEmptyBy(payloadLookup(withEmpty), fw.EmptyKey) {
		t.Error("a nil IPv6 table and a populated one must reach different " +
			"verdicts; if they do not, the v6 keys are not being read at all")
	}
}
