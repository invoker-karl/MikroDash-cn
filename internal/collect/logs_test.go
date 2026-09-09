package collect

import (
	"testing"

	"mikrodash/internal/routeros"
)

// The severity classification, including the two branches no fixture reaches.
//
// The captured router's 500 lines are `info` and `warning` only, so `error` and
// `debug` are unexercised by the differential gate — and the ORDER of the
// branches is not visible there either. A row carries several topics at once, so
// "system,error,warning" has to read as an error and not as a warning: the first
// branch that matches wins, and reordering them would silently reclassify rows
// the page colours.
func TestClassifyLog(t *testing.T) {
	cases := []struct{ topics, want string }{
		{"system,info,account", "info"},
		{"system,error,critical", "error"},
		{"system,critical", "error"},
		{"firewall,warning", "warning"},
		{"debug,dhcp", "debug"},
		{"", "info"},
		{"SYSTEM,ERROR", "error"},           // matched case-insensitively
		{"system,error,warning", "error"},   // error outranks warning
		{"system,warning,debug", "warning"}, // warning outranks debug
		{"interface,link", "info"},          // nothing matched
	}
	for _, c := range cases {
		if got := classifyLog(c.topics); got != c.want {
			t.Errorf("classifyLog(%q) = %q, want %q", c.topics, got, c.want)
		}
	}
}

// The ring buffer drops the OLDEST, and the depth is the live default.
func TestLogRingDropsOldest(t *testing.T) {
	l := &Logs{size: 3}
	for _, m := range []string{"a", "b", "c", "d", "e"} {
		l.push(LogEntry{Message: m})
	}
	got := ""
	for _, e := range l.history {
		got += e.Message
	}
	if got != "cde" {
		t.Errorf("ring holds %q, want %q", got, "cde")
	}
	if n := logHistorySize(); n != 500 {
		t.Errorf("default history size is %d, want 500", n)
	}
}

// ── PHASE 4.1: THE SECOND SEQUENCE DERIVATION ──────────────────────────────
//
// A log line is the clearest case of a fold: each row is an EVENT, it matters
// once, and there is no "current value of the log" to map over. That is also why
// this menu can never back a rolling cache entry — `roscache.unrollable` refuses
// it for exactly this reason.

func TestFoldLogKeepsTheMostRecentLines(t *testing.T) {
	var ring []LogEntry
	for i := 0; i < 10; i++ {
		_, ring = FoldLog(ring, 3, routeros.Reply{
			"message": string(rune('a' + i)), "topics": "info",
		}, int64(i))
	}
	if len(ring) != 3 {
		t.Fatalf("ring holds %d, want 3", len(ring))
	}
	// THE LAST THREE, NOT THE FIRST. A ring that trimmed from the back would
	// freeze on the oldest lines and never show anything that just happened,
	// which is the entire purpose of a log tail.
	want := []string{"h", "i", "j"}
	for i, w := range want {
		if ring[i].Message != w {
			t.Errorf("ring[%d] = %q, want %q — the ring is trimming from the wrong "+
				"end, so the page shows the oldest lines for ever", i, ring[i].Message, w)
		}
	}
}

// TestFoldLogDoesNotWriteThroughToThePrior.
//
// `append` into a slice with spare capacity writes THROUGH to the caller's
// backing array, and these rings always have spare capacity once trimmed. The
// equivalent test for `FoldPing` passed a mutation for a whole round because it
// was written with slice literals, whose capacity equals their length — so this
// one is built with room to spare on purpose.
func TestFoldLogDoesNotWriteThroughToThePrior(t *testing.T) {
	prior := append(make([]LogEntry, 0, 8),
		LogEntry{Message: "one"}, LogEntry{Message: "two"})
	_, next := FoldLog(prior, 10, routeros.Reply{"message": "three"}, 1)

	if len(prior) != 2 {
		t.Errorf("the prior grew to %d", len(prior))
	}
	// The element PAST the prior's length is where an in-place append lands, and
	// a length check alone would miss it.
	if full := prior[:cap(prior)]; full[2].Message != "" {
		t.Errorf("the fold appended into the prior's spare capacity (found %q); the "+
			"caller's history was written through", full[2].Message)
	}
	if len(next) != 3 || next[2].Message != "three" {
		t.Errorf("next = %d entries ending %q", len(next), next[len(next)-1].Message)
	}
}

// TestFoldLogClassifiesFromTopics — the severity is derived, not carried, so a
// line's colour cannot disagree with its topics.
func TestFoldLogClassifiesFromTopics(t *testing.T) {
	e, _ := FoldLog(nil, 10, routeros.Reply{
		"message": "login failure", "topics": "system,error,critical", "time": "12:00:00",
	}, 42)
	if e.Severity == "" {
		t.Error("no severity derived from the topics")
	}
	if e.TS != 42 || e.Time != "12:00:00" || e.Message != "login failure" {
		t.Errorf("entry = %+v; the fold altered fields it should pass through", e)
	}
}

// TestFoldLogWithNoCapIsUnbounded — `entryOf` builds a single entry with size 0
// and must not have its ring trimmed to nothing.
func TestFoldLogWithNoCapIsUnbounded(t *testing.T) {
	var ring []LogEntry
	for i := 0; i < 5; i++ {
		_, ring = FoldLog(ring, 0, routeros.Reply{"message": "x"}, int64(i))
	}
	if len(ring) != 5 {
		t.Errorf("ring holds %d with no cap, want 5 — a zero size must mean "+
			"unbounded, not empty", len(ring))
	}
}
