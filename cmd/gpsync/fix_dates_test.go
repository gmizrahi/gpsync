package main

import (
	"testing"
	"time"
)

// TestFixDates_OverwritesOnlyRowsWhosePathYearDisagrees covers the bug
// where per-year file counts were wrong because captured_at had drifted
// from the file's own path. Three cases: a row whose stored (mtime-drifted) captured_at disagrees
// with its own path -- must be overwritten; a row that already agrees
// -- must be left untouched (no needless churn); and a row with no
// plausible date anywhere in its path -- must fall through unchanged
// too, since there's nothing better to derive.
func TestFixDates_OverwritesOnlyRowsWhosePathYearDisagrees(t *testing.T) {
	db := openInfoTestDB(t)

	drifted := float64(time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC).Unix())
	mustNoErr(t, db.EnsurePending("h-drifted", 100, "image/jpeg",
		"/mnt/c/Photos/2002/2002_07/2002-07-10-01.jpg", &drifted))

	agrees := float64(time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC).Unix())
	mustNoErr(t, db.EnsurePending("h-agrees", 200, "image/jpeg",
		"/mnt/c/Photos/2019/vacation.jpg", &agrees))

	mustNoErr(t, db.EnsurePending("h-no-path-date", 300, "image/jpeg",
		"/mnt/c/Photos/Misc/scan017.jpg", &drifted))

	sum, err := fixDates(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.fixed != 1 {
		t.Errorf("fixed = %d, want 1 (only h-drifted)", sum.fixed)
	}
	if sum.unchanged != 2 {
		t.Errorf("unchanged = %d, want 2 (h-agrees + h-no-path-date)", sum.unchanged)
	}

	row, err := db.GetUpload("h-drifted")
	if err != nil {
		t.Fatal(err)
	}
	if !row.CapturedAt.Valid {
		t.Fatal("h-drifted captured_at is not valid after fix")
	}
	gotYear := time.Unix(int64(row.CapturedAt.Float64), 0).UTC().Year()
	if gotYear != 2002 {
		t.Errorf("h-drifted captured_at year = %d, want 2002 (from its path, not the drifted 2026 mtime)", gotYear)
	}

	unchanged, err := db.GetUpload("h-agrees")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.CapturedAt.Float64 != agrees {
		t.Errorf("h-agrees captured_at = %v, want untouched at %v", unchanged.CapturedAt.Float64, agrees)
	}

	stillDrifted, err := db.GetUpload("h-no-path-date")
	if err != nil {
		t.Fatal(err)
	}
	if stillDrifted.CapturedAt.Float64 != drifted {
		t.Errorf("h-no-path-date captured_at = %v, want untouched at %v (no plausible date in its path)", stillDrifted.CapturedAt.Float64, drifted)
	}
}

// TestFixDates_DryRunWritesNothing proves --dry-run reports what would
// change without actually touching the ledger, matching `gpsync recheck
// --dry-run`'s own precedent.
func TestFixDates_DryRunWritesNothing(t *testing.T) {
	db := openInfoTestDB(t)
	drifted := float64(time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC).Unix())
	mustNoErr(t, db.EnsurePending("h-drifted", 100, "image/jpeg",
		"/mnt/c/Photos/2002/2002_07/2002-07-10-01.jpg", &drifted))

	sum, err := fixDates(db, true)
	if err != nil {
		t.Fatal(err)
	}
	if sum.fixed != 1 {
		t.Errorf("fixed = %d, want 1 (still reported even in dry-run)", sum.fixed)
	}

	row, err := db.GetUpload("h-drifted")
	if err != nil {
		t.Fatal(err)
	}
	if row.CapturedAt.Float64 != drifted {
		t.Errorf("captured_at = %v, want unchanged at %v -- --dry-run must write nothing", row.CapturedAt.Float64, drifted)
	}
}
