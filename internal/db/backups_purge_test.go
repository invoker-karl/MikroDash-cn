package db

import "testing"

// PurgePrunedBackups clears what an older build marked, and nothing else.
//
// ── WHY IT EXISTS ───────────────────────────────────────────────────────────
//
// Retention used to MARK a row whose files it removed and keep it for ever. The
// row was there so the History table could "explain the disappearance", and
// nothing else ever read it — restore, download, raw-serving and diff each
// refuse a pruned row, and the scheduler's `LastBackupRun` cannot see one
// because retention always spares the newest run.
//
// Retention deletes the row now. This clears the tombstones already sitting in
// an installed database, which would otherwise stay for ever describing files
// that have not existed for weeks.
//
// ── WHAT IT MUST NOT TAKE ───────────────────────────────────────────────────
//
// Three kinds of row have no usable files and only ONE of them is a tombstone,
// so a purge written as "delete rows with no stem" would take all three:
//
//	unchanged  the configuration matched, so nothing was stored — and the
//	           newest of these is what the History table shows to answer "did
//	           the schedule actually fire?"
//	failed     a run that errored, which a router failing nightly needs in
//	           order to show that from its own history
//	pruned     something WAS stored and retention removed it
//
// Only `pruned_at` distinguishes them, which is why that is the predicate.
func TestPurgePrunedBackupsTakesOnlyTombstones(t *testing.T) {
	d := openTest(t, t.TempDir())

	ptr := func(s string) *string { return &s }
	mk := func(outcome string, st *string, at int64) int64 {
		id, err := d.RecordBackup(BackupRun{
			RouterID: "r1", TakenAt: at, Outcome: outcome, Source: "schedule",
			Stem: st, Dir: ptr("/data/config-backups/r1"),
		})
		if err != nil {
			t.Fatalf("seeding a %s row: %v", outcome, err)
		}
		return id
	}

	live := mk("changed", ptr("2026-09-07T085234"), 1_788_000_000_000)
	gone := mk("changed", ptr("2026-08-19T191841"), 1_787_000_000_000)
	noChange := mk("unchanged", nil, 1_787_500_000_000)
	failed := mk("failed", nil, 1_787_600_000_000)

	// Only the one retention took is marked.
	if ok, err := d.MarkBackupPruned(gone, 1_787_900_000_000); err != nil || !ok {
		t.Fatalf("marking the tombstone: ok=%v err=%v", ok, err)
	}

	n, err := d.PurgePrunedBackups()
	if err != nil {
		t.Fatalf("purging: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d row(s), want exactly 1 — the marked one", n)
	}

	for _, c := range []struct {
		id   int64
		want bool
		what string
	}{
		{live, true, "a live restore point"},
		{noChange, true, "an unchanged run, which is how the page shows the schedule fired"},
		{failed, true, "a failed run, which a router failing nightly needs to show"},
		{gone, false, "the tombstone"},
	} {
		row, err := d.GetBackup(c.id)
		if err != nil {
			t.Fatalf("reading id %d: %v", c.id, err)
		}
		if got := row != nil; got != c.want {
			verb := "should have been removed and survived"
			if c.want {
				verb = "should have survived and was removed"
			}
			t.Errorf("%s (id %d) %s", c.what, c.id, verb)
		}
	}
}

// AND IT IS IDEMPOTENT, which is what makes it safe at every start.
//
// It runs on each boot from `cmd/mikrodash`. If a second call could remove
// anything, the app would be quietly eating its own history one restart at a
// time — the worst shape this could fail in, because nothing would report it.
func TestPurgePrunedBackupsIsIdempotent(t *testing.T) {
	d := openTest(t, t.TempDir())
	s := "2026-09-07T085234"
	dir := "/data/config-backups/r1"
	if _, err := d.RecordBackup(BackupRun{
		RouterID: "r1", TakenAt: 1_788_000_000_000, Outcome: "changed",
		Source: "schedule", Stem: &s, Dir: &dir,
	}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	first, err := d.PurgePrunedBackups()
	if err != nil {
		t.Fatalf("first purge: %v", err)
	}
	second, err := d.PurgePrunedBackups()
	if err != nil {
		t.Fatalf("second purge: %v", err)
	}
	if first != 0 || second != 0 {
		t.Errorf("purged %d then %d rows from a database with no tombstones; "+
			"both must be 0 or a restart is eating history", first, second)
	}

	rows, err := d.ListBackups("r1", 200)
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("the live row did not survive two purges: %d row(s) left", len(rows))
	}
}
