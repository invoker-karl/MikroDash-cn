package verify

import (
	"regexp"
	"strings"
	"testing"
)

// TestDevicesFocusPrimesTheAlertPool.
//
// ── THE POOL CAN ONLY BE PRIMED BY SOMEBODY ASKING ─────────────────────────
//
// `alertpool.PrimeStats` is deliberately not a poll: a router with alerting and
// reporting both off holds a bare socket and runs no collectors, and reading its
// gauges on a schedule would give back the cost the toggle exists to remove. It
// is taken ONCE, when a browser opens the Devices page, on a connection that is
// already open.
//
// So the whole feature is one call in one handler, and its own unit tests pass
// with that call deleted — they drive `PrimeStats` directly. What comes back is
// the symptom it was added for: a card with a green badge over an empty CPU,
// memory and uptime for the two seconds the overview pool takes to dial. Nothing
// errors, nothing logs, and the page eventually fills in, which is what makes it
// easy to lose and hard to notice.
//
// Read rather than run, like the rejoin check next door: driving `devicesFocus`
// needs a socket, a store, two pools and a router.
func TestDevicesFocusPrimesTheAlertPool(t *testing.T) {
	root := repoRoot(t)
	src := mustRead(t, root+"/internal/server/devices.go")

	i := strings.Index(src, "func (cn *conn) devicesFocus(")
	if i < 0 {
		t.Fatal("devicesFocus is gone from devices.go — this check is reading nothing")
	}
	body := src[i:]
	if j := regexp.MustCompile(`\n(func|type|var|const) `).FindStringIndex(body[1:]); j != nil {
		body = body[:j[0]+1]
	}

	if !strings.Contains(body, "PrimeStats()") {
		t.Error("devicesFocus does not call PrimeStats — a router with alerting " +
			"and reporting off has no collectors, so its card opens with a green " +
			"badge and blank gauges until the overview pool finishes dialling.")
	}

	// ── ORDER IS THE OTHER HALF ───────────────────────────────────────────
	//
	// Priming AFTER the first payload is built primes nothing anyone can see:
	// the frame has already gone with the gap in it, and by the next tick the
	// overview pool is answering. That reads as a working call and fixes nothing.
	prime := strings.Index(body, "PrimeStats()")
	send := strings.Index(body, "cn.sendRoutersStats()")
	if prime >= 0 && send >= 0 && prime > send {
		t.Error("devicesFocus primes the alert pool AFTER sending the first " +
			"routers:stats — the frame the fix exists for has already left.")
	}
}
