package verify

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"mikrodash/internal/session"
)

// TestAlertFeedsMatchesWhatTheRulesConsume.
//
// ── THE DEPENDENCY THAT WAS NEVER WRITTEN DOWN ──────────────────────────────
//
// Six alert rules read six collectors' payloads. Nothing connected the two: the
// rules dispatch on event names in `alertwire`, and the collectors are started
// by a list in `session.go` that was written for viewers. Four of the six
// overlapped by luck; `vpn` and `routing` did not, and were running only because
// `primeAll` gave them a payload and the dormancy probe then resumed them.
//
// That is two unrelated mechanisms holding up a third. Turn either off, or move
// when a probe fires, and VPN and BGP alerts stop firing for the router somebody
// is actually watching — with nothing anywhere to say so.
//
// So the dependency is declared as `session.AlertFeeds` and checked here against
// the events `alertwire` really dispatches on. BOTH DIRECTIONS: a rule reading a
// collector that is not fed fails, and a collector listed as fed that no rule
// reads fails too, because that one costs a poll on every alerting router for
// nothing.
func TestAlertFeedsMatchesWhatTheRulesConsume(t *testing.T) {
	src := mustRead(t, filepath.Join(repoRoot(t), "internal", "alertwire", "wire.go"))

	// THE DISPATCH IS A TYPE SWITCH ON THE PAYLOAD, guarded by the event name --
	// `case *collect.VPNPayload:` followed by `if event != "vpn:update"`. The
	// event guard is what names the dependency, so that is what is read. (The
	// first version of this looked for `case "vpn:update":` and found nothing,
	// which the zero-match guard below caught rather than passing vacuously.)
	caseRe := regexp.MustCompile(`event != "([a-z]+):([a-z-]+)"`)
	// event prefix -> the collector key that emits it. The two differ often
	// enough (ifstatus/ifStatus, conn/conns) that deriving would be guesswork.
	keyOf := map[string]string{
		"ping": "ping", "vpn": "vpn", "ifstatus": "ifStatus",
		"netwatch": "netwatch", "system": "system", "routing": "routing",
	}

	consumed := map[string]bool{}
	for _, m := range caseRe.FindAllStringSubmatch(stripGoComments(src), -1) {
		if key, ok := keyOf[m[1]]; ok {
			consumed[key] = true
		}
	}
	if len(consumed) == 0 {
		t.Fatal("no alert dispatch cases were found — the switch has changed shape and " +
			"this check is reading nothing")
	}

	declared := map[string]bool{}
	for _, k := range session.AlertFeeds {
		declared[k] = true
	}

	var unfed, unused []string
	for k := range consumed {
		if !declared[k] {
			unfed = append(unfed, k)
		}
	}
	for k := range declared {
		if !consumed[k] {
			unused = append(unused, k)
		}
	}
	sort.Strings(unfed)
	sort.Strings(unused)

	if len(unfed) > 0 {
		t.Errorf("the alert rules read %v and session.AlertFeeds does not list them.\n"+
			"Nothing starts those collectors for alerting, so the rule fires only while "+
			"somebody happens to have the right page open — silently, because a rule that "+
			"is never fed looks exactly like a rule with nothing to report.", unfed)
	}
	if len(unused) > 0 {
		t.Errorf("session.AlertFeeds lists %v and no alert rule reads them. Every router "+
			"with alerting on then polls for nothing, which is the cost this project "+
			"exists to remove.", unused)
	}
}

// TestTheAlertFeedIsStartedBecauseAlertingIsOn. The list is only worth having
// while something acts on it: `vpn` and `routing` must be started under the
// alerting switch, not left to the accident that was covering for them.
func TestTheAlertFeedIsStartedBecauseAlertingIsOn(t *testing.T) {
	src := mustRead(t, filepath.Join(repoRoot(t), "internal", "session", "session.go"))
	flat := strings.Join(strings.Fields(stripGoComments(src)), " ")

	if !strings.Contains(flat, "if s.alertsEnabled { if s.eff.Enabled[\"vpn\"] { s.vpn.Start() }") {
		t.Error("vpn and routing are no longer started under the alertsEnabled gate, so " +
			"the alert feed is back to depending on which page somebody opened")
	}
	// And the suspend side, which is the other half: a page blur must not stop a
	// collector the rules still need.
	ws := stripGoComments(mustRead(t, filepath.Join(repoRoot(t), "internal", "server", "ws.go")))
	if !strings.Contains(strings.Join(strings.Fields(ws), " "), "rs.NeededForAlerts(key)") {
		t.Error("the blur guard no longer asks NeededForAlerts, so blurring the VPN page " +
			"suspends a collector four of the six rules depend on")
	}
}
