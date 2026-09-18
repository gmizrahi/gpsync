package engine

import (
	"fmt"
	"time"

	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// FixDatesSummary counts what one pass found and did.
type FixDatesSummary struct {
	Fixed     int
	Unchanged int
}

// FixDates backfills capture dates from the file path for rows whose
// stored date disagrees on the YEAR.
//
// The scanner now prefers a date parsed from the path over filesystem
// mtime, but that only helps NEW scans: EnsurePending is `ON CONFLICT DO
// NOTHING`, so an already-tracked hash never has its captured_at revisited.
// This is what repairs rows the old mtime-only fallback reached, where a
// later backup or re-sync had pushed mtime years away from when the photo
// was actually taken.
//
// Compares by year, not exact timestamp, so a row already close enough --
// from real EXIF, or an mtime that happens to still agree -- is left alone
// and the pass does not churn every row on every run.
//
// Safe to run repeatedly. With dryRun set, nothing is written and the
// summary reports what would change.
func FixDates(db *statedb.DB, dryRun bool) (FixDatesSummary, error) {
	var sum FixDatesSummary
	rows, err := db.AllCaptureDates()
	if err != nil {
		return sum, err
	}
	for _, r := range rows {
		t, ok := scanner.DateFromPath(r.Path)
		if !ok {
			sum.Unchanged++
			continue
		}
		if r.CapturedAt.Valid && time.Unix(int64(r.CapturedAt.Float64), 0).UTC().Year() == t.Year() {
			sum.Unchanged++
			continue
		}
		sum.Fixed++
		if dryRun {
			continue
		}
		if err := db.UpdateCapturedAt(r.SHA256, float64(t.Unix())); err != nil {
			return sum, fmt.Errorf("updating capture date for %s: %w", r.Path, err)
		}
	}
	return sum, nil
}
