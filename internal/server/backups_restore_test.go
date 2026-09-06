package server

import (
	"os"
	"strings"
	"testing"
)

// `restoreBase` and `portSuffix` are the halves of "where can the ROUTER reach
// us", which is the question the operator's base-URL decision answered. The rest
// of the handler needs a live session and is covered by the identity and
// mismatch rules being separate, testable pieces.

func TestPortSuffix(t *testing.T) {
	cases := []struct{ listen, want string }{
		{":3082", ":3082"},
		{"0.0.0.0:3082", ":3082"},
		{"127.0.0.1:8080", ":8080"},
		// An IPv6 listen address still ends at its port.
		{"[::]:3082", ":3082"},
		// A bare port with no colon is treated as one.
		{"3082", ":3082"},
		{"", ""},
	}
	for _, c := range cases {
		if got := portSuffix(c.listen); got != c.want {
			t.Errorf("portSuffix(%q) = %q, want %q", c.listen, got, c.want)
		}
	}
}

// The URL a router is handed must be the REAL path — it cannot be told about a
// staging prefix, and this is the pairing that would break silently if the route
// moved.
func TestBackupRawURLMatchesTheRegisteredRoute(t *testing.T) {
	got := backupRawURL(42, "abc123")
	want := "/api/backups/42/raw?t=abc123"
	if got != want {
		t.Errorf("backupRawURL = %q, want %q", got, want)
	}
	// And the route it points at is registered at the same shape.
	if backupRawPath != "/api/backups/{id}/raw" {
		t.Errorf("the URL handed to a router (%q) and the registered path (%q) have diverged",
			got, backupRawPath)
	}
}

// The audit note records whether a version mismatch was ACCEPTED, because that
// is the difference between "restored onto the same build" and "the operator
// was asked and said yes" — and only the second one explains a surprise later.
func TestAcceptedNote(t *testing.T) {
	if acceptedNote(false) != "" {
		t.Errorf("an unaccepted restore added a note: %q", acceptedNote(false))
	}
	if acceptedNote(true) == "" {
		t.Error("an accepted version mismatch left no trace in the audit note")
	}
}

// The destination filename is fixed and overwritten every restore. Pinned
// because a router-side name that drifted from the live one would leave two
// files on the device and restore the wrong one.
func TestRestoreDestinationMatchesTheLiveName(t *testing.T) {
	if restoreDst != "mikrodash-restore.backup" {
		t.Errorf("restoreDst = %q; the live app writes mikrodash-restore.backup", restoreDst)
	}
}

// ── THE ROUTERBOARD READ MUST NOT FAIL THE RESTORE ─────────────────────────
//
// `/system/routerboard` is optional hardware. A CHR and an x86 install have no
// routerboard and answer the menu with an error, and `readIdentity` used to
// return that error, which `restore` turned into a flat "failed". A virtual
// router could take a backup and never put one back.
//
// `internal/backups/runner.go` had already reached the opposite conclusion for
// the BACKUP half — "refusing the whole backup because a virtual router has no
// serial number would be refusing it for being a virtual router" — and
// `backups.CheckRestore` compares serials only when BOTH sides are non-empty,
// under a header naming CHR. So the tolerance was safe and simply missing on one
// of the two paths.
//
// ── WHY A SOURCE PIN, AND WHAT ACTUALLY PROVES IT ──────────────────────────
//
// The property is WHICH OF TWO READS may fail, and `cn.rsession` is a concrete
// *session.Session — a behavioural test would have to stand up a session with a
// fake dial just to assert an error is swallowed, and would still not say which
// read swallowed it. This pins the asymmetry directly.
//
// The behavioural proof is a restore onto a real CHR, which is the one thing no
// test in this repository can do. See the plan for issue #121.
func TestTheRouterboardReadDoesNotFailARestore(t *testing.T) {
	b, err := os.ReadFile("backups_restore.go")
	if err != nil {
		t.Fatalf("reading backups_restore.go: %v", err)
	}
	src := string(b)

	i := strings.Index(src, "func (cn *conn) readIdentity(")
	if i < 0 {
		t.Fatal("readIdentity is gone from backups_restore.go — this check is reading nothing")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
		body = body[:j+1]
	}

	rb := strings.Index(body, "/system/routerboard/print")
	res := strings.Index(body, "/system/resource/print")
	if rb < 0 || res < 0 {
		t.Fatal("readIdentity no longer reads both menus — either it was reshaped, " +
			"in which case fix this check, or one of the two reads has gone")
	}
	if rb > res {
		t.Fatal("the reads swapped order; this check slices the body between them")
	}

	// Between the routerboard read and the resource read there must be NO
	// early return: that stretch is where the tolerated failure lives.
	if strings.Contains(body[rb:res], `return "", "", `) {
		t.Error("readIdentity returns early on the /system/routerboard read.\n" +
			"That menu does not exist on a CHR or an x86 install, so this " +
			"refuses a restore for the router being virtual. Let it fail soft " +
			"and leave the serial empty — CheckRestore already treats an " +
			"unknown serial as a match, and says so.")
	}

	// And the resource read must STILL be fatal. Every router has
	// /system/resource; one that cannot answer it is broken rather than
	// virtual, and restoring onto a device that cannot say what it is running
	// is not something to do quietly.
	if !strings.Contains(body[res:], `return "", "", err`) {
		t.Error("readIdentity no longer fails on the /system/resource read. " +
			"That one is not optional: it is how the version guard knows what " +
			"it is restoring onto.")
	}
}
