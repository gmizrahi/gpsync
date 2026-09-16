package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func findingFor(t *testing.T, rep Report, check string) Finding {
	t.Helper()
	for _, f := range rep.Findings {
		if f.Check == check {
			return f
		}
	}
	t.Fatalf("no finding for check %q", check)
	return Finding{}
}

func TestDiagnose_CleanLedger_IsHealthy(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jpg")
	mustNoErr(t, os.WriteFile(p, []byte("x"), 0o644))
	mustNoErr(t, db.EnsurePending("h1", 1, "image/jpeg", p, nil))

	rep, err := Diagnose(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Healthy() {
		t.Errorf("expected a clean bill of health, got problems: %+v", rep.Problems())
	}
	// Every check must report SOMETHING, healthy or not -- a check that
	// silently vanishes when clean can't be distinguished from one that
	// was accidentally dropped.
	if len(rep.Findings) < 5 {
		t.Errorf("len(Findings) = %d, want every check represented even when clean", len(rep.Findings))
	}
}

func TestDiagnose_RetryableBacklog_IsFoundAndRequeued(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jpg")
	mustNoErr(t, os.WriteFile(p, []byte("x"), 0o644))
	mustNoErr(t, db.EnsurePending("h1", 1, "image/jpeg", p, nil))
	mustNoErr(t, db.MarkFailed("h1", false, "throttle", "429"))

	rep, err := Diagnose(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if f := findingFor(t, rep, CheckRetryableBacklog); f.Count != 1 || !f.Fixable {
		t.Fatalf("retryable finding = %+v, want Count 1 and fixable", f)
	}

	if _, err := Repair(db, time.Now()); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetUpload("h1")
	if err != nil || row == nil {
		t.Fatal(err)
	}
	if row.Status != "pending" {
		t.Errorf("status = %q, want pending after repair", row.Status)
	}
}

func TestDiagnose_StuckRun_OnlyFlaggedOnceStale(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.RunStart("upload", 0, 5))

	// A run that just updated is healthy, not stuck -- a long throttle
	// pause is silent by design, so a tight threshold would flag a
	// perfectly fine run.
	rep, err := Diagnose(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if f := findingFor(t, rep, CheckStuckRun); f.Count != 0 {
		t.Errorf("a freshly-started run was flagged as stuck: %+v", f)
	}

	// Far enough in the future that the same row is unambiguously stale.
	later := time.Now().Add(stuckRunAfter + time.Hour)
	rep, err = Diagnose(db, later)
	if err != nil {
		t.Fatal(err)
	}
	f := findingFor(t, rep, CheckStuckRun)
	if f.Count != 1 || !f.Fixable {
		t.Fatalf("stale run finding = %+v, want Count 1 and fixable", f)
	}

	if _, err := Repair(db, later); err != nil {
		t.Fatal(err)
	}
	run, err := db.RunGet()
	if err != nil || run == nil {
		t.Fatal(err)
	}
	if run.Status == "running" {
		t.Error("the stale run should have been closed out, not left active")
	}
}

func TestDiagnose_IsReadOnly(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	mustNoErr(t, db.EnsurePending("h-gone", 1, "image/jpeg", filepath.Join(dir, "gone.jpg"), nil))
	mustNoErr(t, db.EnsurePending("h2", 1, "image/jpeg", filepath.Join(dir, "gone2.jpg"), nil))
	mustNoErr(t, db.MarkFailed("h2", false, "throttle", "429"))

	// Diagnose must never repair anything on its own -- it's documented as
	// safe to run against a live ledger while uploads are in flight.
	for i := 0; i < 2; i++ {
		if _, err := Diagnose(db, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if row, err := db.GetUpload("h-gone"); err != nil || row == nil {
		t.Error("Diagnose deleted a row -- it must be read-only")
	}
	row, err := db.GetUpload("h2")
	if err != nil || row == nil {
		t.Fatal(err)
	}
	if row.Status != "failed_retryable" {
		t.Errorf("status = %q, want failed_retryable untouched -- Diagnose must not requeue", row.Status)
	}
}

func TestDiagnose_PermanentFailuresAreReportedNotFixed(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "a.jpg")
	mustNoErr(t, os.WriteFile(p, []byte("x"), 0o644))
	mustNoErr(t, db.EnsurePending("h1", 1, "image/jpeg", p, nil))
	mustNoErr(t, db.MarkFailed("h1", true, "permanent", "unsupported"))

	rep, err := Diagnose(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	perm := findingFor(t, rep, CheckPermanentFailures)
	if perm.Count != 1 {
		t.Errorf("permanent-failure count = %d, want 1", perm.Count)
	}
	// Deliberately defers to a better-suited mechanism rather than acting
	// under a generic --fix.
	if perm.Fixable {
		t.Error("permanent failures must NOT be auto-fixed -- `gpsync recheck` judges them individually")
	}
	if perm.Remedy == "" {
		t.Error("a non-fixable finding still has to tell the user what to do")
	}
}

// TestDiagnose_ReportsConfirmedMissingButNeverRemovesThem: doctor reports
// what ResolveMissingFiles confirmed and points at recheck --missing, but
// --fix must not forget anything -- that decision re-checks each file first
// and belongs to recheck alone.
func TestDiagnose_ReportsConfirmedMissingButNeverRemovesThem(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	gone := filepath.Join(dir, "gone.jpg")
	mustNoErr(t, db.EnsurePending("gone", 1, "image/jpeg", gone, nil))
	mustNoErr(t, db.MarkUploaded("gone", "media-1", ""))
	mustNoErr(t, db.MarkMissing("gone", 1))
	mustNoErr(t, db.ConfirmMissing("gone", 2))
	// Flagged but not confirmed: not reported.
	mustNoErr(t, db.EnsurePending("flagged", 1, "image/jpeg", filepath.Join(dir, "flagged.jpg"), nil))
	mustNoErr(t, db.MarkMissing("flagged", 1))

	rep, err := Diagnose(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	f := findingFor(t, rep, CheckMissingSourceFile)
	if f.Count != 1 || f.Fixable || !strings.Contains(f.Remedy, "recheck --missing") {
		t.Fatalf("finding = %+v, want 1 confirmed, not fixable, pointing at recheck --missing", f)
	}
	if _, err := Repair(db, time.Now()); err != nil {
		t.Fatal(err)
	}
	if row, err := db.GetUpload("gone"); err != nil || row == nil {
		t.Error("Repair removed a confirmed-missing entry; only recheck --missing may")
	}
}
