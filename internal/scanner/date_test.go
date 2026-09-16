package scanner

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDateFromPath_SeparatedDate proves the most precise match: a real
// YYYY-MM-DD embedded in the filename, matching this project's own
// real-world source data (the exact bug report this fixes): "i assure
// you, your numbers are wrong for the years; just perform du
// /mnt/c/Photos/2002 to see that we have much more files than what you
// counted" -- confirmed against the real library that files named e.g.
// "2002-07-10-01.jpg" had a filesystem mtime years removed from their own
// stated date.
func TestDateFromPath_SeparatedDate(t *testing.T) {
	tm, ok := DateFromPath("/mnt/c/Photos/2002/2002_07/2002-07-10-01.jpg")
	if !ok {
		t.Fatal("DateFromPath = false, want true")
	}
	if tm.Year() != 2002 || tm.Month() != time.July || tm.Day() != 10 {
		t.Errorf("DateFromPath = %v, want 2002-07-10", tm)
	}
}

// TestDateFromPath_CompactDate covers the phone/app-export convention
// (WhatsApp, Google's own camera app, etc.) of a bare YYYYMMDD run in the
// filename.
func TestDateFromPath_CompactDate(t *testing.T) {
	tm, ok := DateFromPath(`C:\Photos\Whatsapp\IMG-20220701-WA0060.jpg`)
	if !ok {
		t.Fatal("DateFromPath = false, want true")
	}
	if tm.Year() != 2022 || tm.Month() != time.July || tm.Day() != 1 {
		t.Errorf("DateFromPath = %v, want 2022-07-01", tm)
	}
}

// TestDateFromPath_YearOnlyFolder covers a library organized as a bare
// "YYYY" folder with no further date detail in the filename -- falls back
// to January 1st of that year, noon UTC.
func TestDateFromPath_YearOnlyFolder(t *testing.T) {
	tm, ok := DateFromPath("/lib/2019/vacation-photo.jpg")
	if !ok {
		t.Fatal("DateFromPath = false, want true")
	}
	if tm.Year() != 2019 || tm.Month() != time.January || tm.Day() != 1 {
		t.Errorf("DateFromPath = %v, want 2019-01-01", tm)
	}
}

// TestDateFromPath_NoPlausibleDate proves paths with nothing date-shaped,
// or only an implausible 4-digit run, correctly report no match --
// callers must fall back to something else (EXIF/mtime), not a guess.
func TestDateFromPath_NoPlausibleDate(t *testing.T) {
	tests := []string{
		"/lib/no-date-here/photo.jpg",
		"/lib/scan017.jpg",
		"/lib/9999/photo.jpg", // not a real year
	}
	for _, p := range tests {
		if _, ok := DateFromPath(p); ok {
			t.Errorf("DateFromPath(%q) = true, want false (nothing plausible)", p)
		}
	}
}

// TestDateFromPath_InvalidFullDateFallsBackToYearOnly proves a
// full-date-SHAPED match that isn't actually a valid calendar date
// (month 13, day 40) doesn't just fail outright -- it correctly falls
// through to the year-only heuristic, since the leading 4 digits are
// still a perfectly plausible year on their own.
func TestDateFromPath_InvalidFullDateFallsBackToYearOnly(t *testing.T) {
	tm, ok := DateFromPath("/lib/2002-13-40/photo.jpg")
	if !ok {
		t.Fatal("DateFromPath = false, want true (year-only fallback)")
	}
	if tm.Year() != 2002 {
		t.Errorf("DateFromPath = %v, want year 2002", tm)
	}
}

// TestCaptureDate_PrefersPathOverMtime is the actual regression this
// whole fix addresses: a real file with no usable EXIF (a plain text
// file standing in for one with unreadable metadata) but a date-shaped
// filename must use that path date, not its own (here deliberately
// drifted-forward) filesystem mtime.
func TestCaptureDate_PrefersPathOverMtime(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "2002", "2002_07")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "2002-07-10-01.jpg")
	if err := os.WriteFile(p, []byte("not a real jpeg, no EXIF"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Simulate the exact real-world drift: mtime set far in the future
	// relative to the file's own stated date.
	drifted := time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, drifted, drifted); err != nil {
		t.Fatal(err)
	}

	got := captureDate(p)
	if got == nil {
		t.Fatal("captureDate = nil, want a path-derived date")
	}
	gotTime := time.Unix(int64(*got), 0).UTC()
	if gotTime.Year() != 2002 {
		t.Errorf("captureDate = %v (year %d), want year 2002 (from the path, not the drifted 2026 mtime)", gotTime, gotTime.Year())
	}
}
