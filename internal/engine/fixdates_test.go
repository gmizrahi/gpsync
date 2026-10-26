package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

func seedDated(t *testing.T, db *statedb.DB, sha, path string, captured *float64) {
	t.Helper()
	if err := db.EnsurePending(sha, 10, "image/jpeg", path, captured); err != nil {
		t.Fatal(err)
	}
}

// The bug this repairs: a later backup pushed mtime years past when the
// photo was taken, and the path says 2002.
func TestFixDates_CorrectsAYearThatDisagreesWithThePath(t *testing.T) {
	db := openTestDB(t)
	drifted := float64(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Unix())
	seedDated(t, db, "h1", filepath.Join("C:", "Photos", "2002", "2002-07-10-01.jpg"), &drifted)

	sum, err := FixDates(db, false)
	if err != nil {
		t.Fatalf("FixDates: %v", err)
	}
	if sum.Fixed != 1 {
		t.Fatalf("Fixed = %d, want 1", sum.Fixed)
	}
	rows, err := db.AllCaptureDates()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.SHA256 == "h1" {
			if !r.CapturedAt.Valid {
				t.Fatal("captured_at was cleared rather than corrected")
			}
			if y := time.Unix(int64(r.CapturedAt.Float64), 0).UTC().Year(); y != 2002 {
				t.Errorf("year = %d, want 2002", y)
			}
		}
	}
}

// Compares by year on purpose, so a run does not churn rows it did not
// need to touch.
func TestFixDates_LeavesAnAgreeingYearAlone(t *testing.T) {
	db := openTestDB(t)
	agrees := float64(time.Date(2002, 11, 30, 0, 0, 0, 0, time.UTC).Unix())
	seedDated(t, db, "h2", filepath.Join("C:", "Photos", "2002", "2002-07-10-01.jpg"), &agrees)

	sum, err := FixDates(db, false)
	if err != nil {
		t.Fatalf("FixDates: %v", err)
	}
	if sum.Fixed != 0 {
		t.Errorf("Fixed = %d, want 0 -- the year already agrees", sum.Fixed)
	}
}

// The dry run is what the dashboard button's count comes from, so it must
// report the same number without writing anything.
func TestFixDates_DryRunCountsButWritesNothing(t *testing.T) {
	db := openTestDB(t)
	drifted := float64(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).Unix())
	seedDated(t, db, "h3", filepath.Join("C:", "Photos", "2002", "2002-07-10-01.jpg"), &drifted)

	dry, err := FixDates(db, true)
	if err != nil {
		t.Fatalf("dry FixDates: %v", err)
	}
	if dry.Fixed != 1 {
		t.Fatalf("dry Fixed = %d, want 1", dry.Fixed)
	}
	rows, err := db.AllCaptureDates()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.SHA256 == "h3" && time.Unix(int64(r.CapturedAt.Float64), 0).UTC().Year() != 2026 {
			t.Error("the dry run wrote to the ledger")
		}
	}
	// And the real pass still reports the same count.
	wet, err := FixDates(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if wet.Fixed != dry.Fixed {
		t.Errorf("real pass fixed %d, dry run said %d", wet.Fixed, dry.Fixed)
	}
}

// #73: rows dated by the year-only rule before a "2024_05" folder could be
// read are refined to that month -- but only those: a real time in the same
// year is left alone, as is a year-only date whose path still says no more,
// and a second run changes nothing.
func TestFixDates_RefinesTheYearOnlyDateWhenThePathSaysMore(t *testing.T) {
	db := openTestDB(t)
	yearOnly := float64(time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC).Unix())
	realTime := float64(time.Date(2024, 1, 1, 9, 15, 0, 0, time.UTC).Unix())
	seedDated(t, db, "month", filepath.Join("C:", "Photos", "2024_05 Trip", "IMG_1.jpg"), &yearOnly)
	seedDated(t, db, "exif", filepath.Join("C:", "Photos", "2024_05 Trip", "IMG_2.jpg"), &realTime)
	seedDated(t, db, "year", filepath.Join("C:", "Photos", "2024", "IMG_3.jpg"), &yearOnly)

	sum, err := FixDates(db, false)
	if err != nil || sum.Fixed != 1 {
		t.Fatalf("FixDates = %+v, %v; want exactly the year-only row in the month folder fixed", sum, err)
	}
	want := map[string]time.Time{
		"month": time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC),
		"exif":  time.Unix(int64(realTime), 0).UTC(),
		"year":  time.Unix(int64(yearOnly), 0).UTC(),
	}
	rows, err := db.AllCaptureDates()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if got := time.Unix(int64(r.CapturedAt.Float64), 0).UTC(); !got.Equal(want[r.SHA256]) {
			t.Errorf("%s: %v, want %v", r.SHA256, got, want[r.SHA256])
		}
	}
	if sum, _ := FixDates(db, false); sum.Fixed != 0 {
		t.Errorf("second run fixed %d; want 0", sum.Fixed)
	}
}
