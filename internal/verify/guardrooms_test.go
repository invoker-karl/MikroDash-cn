package verify

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoBlurGuardNamesARoomItself is the other half of 4.2's completeness rule.
//
// ── WHAT THIS REPLACED, AND WHY THE REPLACEMENT IS BETTER ───────────────────
//
// Until 4.2 this test compared two lists: the rooms `pageBlur` waited on, spelled
// out by hand at each call site, against the rooms the collector emitted to. They
// had disagreed FIVE times — dhcpNetworks, bandwidth, vpn, firewall, and routing
// on 2026-08-31 — each time as a dashboard card that silently stopped updating
// for anybody who had visited the owning page and left.
//
// Comparing them was the best available check while there were two lists. Now
// there is one: `collect.Others(collector, blurredPage)` derives the wait list
// from the declaration, so the two CANNOT disagree.
//
// What is left to check is COMPLETENESS — that no call site went back to writing
// its own list. That is a weaker-sounding property and a stronger position: the
// old check could only catch a disagreement after somebody wrote one, and this
// one refuses the shape that makes disagreement possible.
func TestNoBlurGuardNamesARoomItself(t *testing.T) {
	src := mustRead(t, filepath.Join(repoRoot(t), "internal", "server", "ws.go"))

	// A room literal handed to the suspend helper, in either of its two forms.
	lit := regexp.MustCompile(`suspendIfNoRoomOccupied\([^)]*\[\]string\{`)
	flat := strings.Join(strings.Fields(stripGoComments(src)), " ")
	if m := lit.FindAllString(flat, -1); len(m) > 0 {
		t.Errorf("%d blur guard(s) still name their own rooms: %v\n"+
			"Rooms are declared in internal/collect/rooms.go and the wait list comes "+
			"from collect.Others(). A hand-written list here is the second statement "+
			"that this phase removed, and it is how a card silently stops updating.",
			len(m), m)
	}

	// And the derivation must actually be in use, or this check passes by
	// looking at a file that no longer guards anything.
	if n := strings.Count(flat, "collect.Others("); n < 8 {
		t.Errorf("ws.go calls collect.Others %d times, expected at least 8. The guards "+
			"have stopped deriving their rooms and this check would not notice.", n)
	}
}

// stripGoComments drops // lines, so a comment describing the shape this test
// forbids does not fail it.
//
// The third time this trap has been hit here: the credential scanner reading a
// comment about proplists, a dormancy gate reading its own explanation, and this.
func stripGoComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
