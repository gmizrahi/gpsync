package engine

import (
	"strconv"
	"testing"
	"time"
)

// TestBuildStatistics_PathYearWinsOverDriftedMtime covers per-year counts
// coming out wrong. Verified against a real library: files with no
// readable EXIF fall back to
// scanner.captureDate()'s filesystem-mtime path, and mtime had drifted
// to 2025/2026 for files whose OWN FILENAMES say "2002-07-10" -- a
// later copy/backup/migration touched them long after the photo was
// actually taken. yearFromPath must win over such a captured_at when
// the path itself contains a plausible year.
func TestBuildStatistics_PathYearWinsOverDriftedMtime(t *testing.T) {
	db := openTestDB(t)
	drifted := float64(time.Date(2026, 3, 6, 0, 0, 0, 0, time.UTC).Unix()) // the drifted mtime, not the real capture date
	mustNoErr(t, db.EnsurePending("h1", 100, "image/jpeg",
		"/mnt/c/Photos/2002/2002_07/2002-07-10-01.jpg", &drifted))
	mustNoErr(t, db.EnsurePending("h2", 200, "image/jpeg",
		"/mnt/c/Photos/2002/2002_12/2002-12-01-001.jpg", &drifted))
	// A file with no path-embedded year at all still falls back to
	// captured_at normally.
	mustNoErr(t, db.EnsurePending("h3", 300, "image/jpeg", "/mnt/c/Photos/Misc/scan017.jpg", &drifted))

	s, err := BuildStatistics(db)
	if err != nil {
		t.Fatal(err)
	}

	byLabel := map[string]CountStat{}
	for _, y := range s.ByYear {
		byLabel[y.Label] = y
	}
	if got := byLabel["2002"]; got.Count != 2 || got.Bytes != 300 {
		t.Errorf(`ByYear["2002"] = %+v, want count=2 bytes=300 (both path-dated files, winning over their drifted 2026 mtime)`, got)
	}
	if got := byLabel["2026"]; got.Count != 1 || got.Bytes != 300 {
		t.Errorf(`ByYear["2026"] = %+v, want count=1 bytes=300 (the one file with no path-embedded year, falling back to captured_at)`, got)
	}
}

// TestYearFromPath covers the extraction helper directly: which paths
// yield a year, which fall through to "no match", and that an
// implausible 4-digit run (not a real year) is rejected the same as
// TestBuildStatistics_ImplausibleCaptureYearFoldsIntoUnknown's
// captured_at case.
func TestYearFromPath(t *testing.T) {
	tests := []struct {
		path     string
		wantYear int
		wantOK   bool
	}{
		{"/mnt/c/Photos/2002/2002_07/2002-07-10-01.jpg", 2002, true},
		{`C:\Photos\2019\2019_06 City Trip\photo.jpg`, 2019, true},
		{"/lib/no-year-here/photo.jpg", 0, false},
		{"/lib/scan017.jpg", 0, false},
		{"/lib/9999/photo.jpg", 0, false}, // not a plausible year
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			y, ok := yearFromPath(tt.path)
			if ok != tt.wantOK || (ok && y != tt.wantYear) {
				t.Errorf("yearFromPath(%q) = (%d, %v), want (%d, %v)", tt.path, y, ok, tt.wantYear, tt.wantOK)
			}
		})
	}
}

// TestBuildStatistics_ImplausibleCaptureYearFoldsIntoUnknown covers a
// year like 4500 appearing on the chart: genuine garbage EXIF data from a
// real source file, not a scanner arithmetic bug. Any year outside a
// generous plausible range folds into the same "Unknown" bucket as a
// missing capture date, rather than getting its own bar that dwarfs
// (or, for something absurdly far off, gets dwarfed alongside) every
// real year in the chart.
func TestBuildStatistics_ImplausibleCaptureYearFoldsIntoUnknown(t *testing.T) {
	db := openTestDB(t)

	future := float64(time.Date(4500, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
	tooOld := float64(time.Date(1850, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
	real := float64(time.Date(2022, 5, 1, 0, 0, 0, 0, time.UTC).Unix())

	mustNoErr(t, db.EnsurePending("h-future", 100, "image/jpeg", "/lib/a.jpg", &future))
	mustNoErr(t, db.EnsurePending("h-old", 200, "image/jpeg", "/lib/b.jpg", &tooOld))
	mustNoErr(t, db.EnsurePending("h-real", 300, "image/jpeg", "/lib/c.jpg", &real))
	mustNoErr(t, db.EnsurePending("h-none", 400, "image/jpeg", "/lib/d.jpg", nil))

	s, err := BuildStatistics(db)
	if err != nil {
		t.Fatal(err)
	}

	for _, y := range s.ByYear {
		if y.Label == "4500" || y.Label == "1850" {
			t.Fatalf("ByYear contains an implausible year bucket %q, want it folded into Unknown: %+v", y.Label, s.ByYear)
		}
	}
	if len(s.ByYear) != 2 {
		t.Fatalf("ByYear has %d buckets, want exactly 2 (2022, Unknown): %+v", len(s.ByYear), s.ByYear)
	}
	if s.ByYear[0].Label != "2022" || s.ByYear[0].Count != 1 {
		t.Errorf("ByYear[0] = %+v, want 2022/count=1", s.ByYear[0])
	}
	if s.ByYear[1].Label != "Unknown" || s.ByYear[1].Count != 3 {
		t.Errorf("ByYear[1] = %+v, want Unknown/count=3 (future + too-old + no-date)", s.ByYear[1])
	}
}

// TestBuildStatistics_AggregatesByKindExtensionAndYear proves the core
// aggregation: totals, the fixed-order Photos/Videos/Other kind buckets,
// both extension sort orders, and the capture-year bucketing (including
// the "Unknown" fallback for files with no capture date).
func TestBuildStatistics_AggregatesByKindExtensionAndYear(t *testing.T) {
	db := openTestDB(t)

	y2024 := float64(time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC).Unix())
	y2023 := float64(time.Date(2023, 3, 1, 0, 0, 0, 0, time.UTC).Unix())

	mustNoErr(t, db.EnsurePending("h1", 1000, "image/jpeg", "/lib/a.jpg", &y2024))
	mustNoErr(t, db.EnsurePending("h2", 2000, "image/jpeg", "/lib/b.jpg", &y2024))
	mustNoErr(t, db.EnsurePending("h3", 3000, "video/mp4", "/lib/c.mp4", &y2023))
	mustNoErr(t, db.EnsurePending("h4", 400, "application/octet-stream", "/lib/d.xyz", nil)) // unrecognized ext, no capture date

	s, err := BuildStatistics(db)
	if err != nil {
		t.Fatal(err)
	}

	if s.TotalFiles != 4 {
		t.Errorf("TotalFiles = %d, want 4", s.TotalFiles)
	}
	if want := int64(1000 + 2000 + 3000 + 400); s.TotalBytes != want {
		t.Errorf("TotalBytes = %d, want %d", s.TotalBytes, want)
	}
	if want := float64(6400) / 4; s.AvgBytes != want {
		t.Errorf("AvgBytes = %v, want %v", s.AvgBytes, want)
	}
	if s.ExtensionCount != 3 { // jpg, mp4, xyz
		t.Errorf("ExtensionCount = %d, want 3", s.ExtensionCount)
	}

	if len(s.ByKind) != 3 {
		t.Fatalf("ByKind has %d entries, want exactly 3 (Photos, Videos, Other)", len(s.ByKind))
	}
	if s.ByKind[0].Label != "Photos" || s.ByKind[0].Count != 2 || s.ByKind[0].Bytes != 3000 {
		t.Errorf("ByKind[0] = %+v, want Photos/2/3000", s.ByKind[0])
	}
	if s.ByKind[1].Label != "Videos" || s.ByKind[1].Count != 1 || s.ByKind[1].Bytes != 3000 {
		t.Errorf("ByKind[1] = %+v, want Videos/1/3000", s.ByKind[1])
	}
	if s.ByKind[2].Label != "Other" || s.ByKind[2].Count != 1 || s.ByKind[2].Bytes != 400 {
		t.Errorf("ByKind[2] = %+v, want Other/1/400", s.ByKind[2])
	}

	// ByExtSize sorted by bytes descending: mp4 (3000) then jpg (3000,
	// tie broken alphabetically) then xyz (400) -- "jpg" < "mp4"
	// alphabetically, so on a tie jpg comes first.
	if len(s.ByExtSize) != 3 || s.ByExtSize[0].Label != "jpg" || s.ByExtSize[0].Bytes != 3000 {
		t.Errorf("ByExtSize[0] = %+v, want jpg/3000 (tie with mp4 broken alphabetically)", s.ByExtSize[0])
	}
	// ByExtCount sorted by count descending: jpg (2) first.
	if len(s.ByExtCount) != 3 || s.ByExtCount[0].Label != "jpg" || s.ByExtCount[0].Count != 2 {
		t.Errorf("ByExtCount[0] = %+v, want jpg/count=2", s.ByExtCount[0])
	}

	if len(s.ByYear) != 3 {
		t.Fatalf("ByYear has %d entries, want 3 (2023, 2024, Unknown)", len(s.ByYear))
	}
	if s.ByYear[0].Label != "2023" || s.ByYear[1].Label != "2024" {
		t.Errorf("ByYear order = [%s, %s, %s], want 2023, 2024, Unknown (chronological, Unknown last)",
			s.ByYear[0].Label, s.ByYear[1].Label, s.ByYear[2].Label)
	}
	if s.ByYear[2].Label != "Unknown" || s.ByYear[2].Count != 1 {
		t.Errorf("ByYear[2] = %+v, want Unknown/count=1", s.ByYear[2])
	}
}

// TestBuildStatistics_IncludesAllStatuses proves the Statistics page
// describes the WHOLE library (matching AllSizes), not just pending
// files -- distinct from SumPendingSizeUnder-based figures elsewhere.
func TestBuildStatistics_IncludesAllStatuses(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h1", 100, "image/jpeg", "/lib/a.jpg", nil))
	mustNoErr(t, db.EnsurePending("h2", 200, "image/jpeg", "/lib/b.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h2", "media-1", ""))
	mustNoErr(t, db.EnsurePending("h3", 300, "image/jpeg", "/lib/c.jpg", nil))
	mustNoErr(t, db.MarkFailed("h3", true, "UNSUPPORTED_EXTENSION", "bad"))

	s, err := BuildStatistics(db)
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalFiles != 3 {
		t.Errorf("TotalFiles = %d, want 3 (pending + uploaded + failed)", s.TotalFiles)
	}
	if s.TotalBytes != 600 {
		t.Errorf("TotalBytes = %d, want 600", s.TotalBytes)
	}
}

// TestBuildStatistics_EmptyLedger proves an empty library doesn't divide
// by zero computing the average, and returns empty (not nil-panicking)
// slices.
func TestBuildStatistics_EmptyLedger(t *testing.T) {
	db := openTestDB(t)

	s, err := BuildStatistics(db)
	if err != nil {
		t.Fatal(err)
	}
	if s.TotalFiles != 0 || s.AvgBytes != 0 {
		t.Errorf("TotalFiles/AvgBytes = %d/%v, want 0/0", s.TotalFiles, s.AvgBytes)
	}
	if len(s.ByKind) != 3 {
		t.Errorf("ByKind = %v, want 3 zero-valued entries (Photos, Videos, Other) even with no data", s.ByKind)
	}
}

// TestBuildStatistics_ExtensionTablesCappedButExtensionCountStaysTrue
// covers the capped extension tables: ByExtCount/ByExtSize are capped to
// TestBuildStatistics_ManyYearsFoldOldestIntoOlderBucket covers the
// fixed-width year chart, which overlapped on a ~31-year library. A cap
// plus one "Older" bucket was chosen over a scrollbar. Proves the oldest
// years
// beyond the cap get merged into one leading "Older" bucket (count AND
// bytes summed), the most recent statisticsYearChartLimit years stay
// individual, and "Unknown" -- unrelated to the cap -- is unaffected and
// stays last.
func TestBuildStatistics_ManyYearsFoldOldestIntoOlderBucket(t *testing.T) {
	db := openTestDB(t)
	startYear := 1990
	totalYears := statisticsYearChartLimit + 5
	for i := 0; i < totalYears; i++ {
		year := startYear + i
		captured := float64(time.Date(year, 6, 1, 0, 0, 0, 0, time.UTC).Unix())
		mustNoErr(t, db.EnsurePending("h"+string(rune('a'+i)), int64(i+1), "image/jpeg",
			"/lib/f"+string(rune('a'+i))+".jpg", &captured))
	}
	// One file with no capture date, to prove Unknown survives untouched.
	mustNoErr(t, db.EnsurePending("h-none", 999, "image/jpeg", "/lib/none.jpg", nil))

	s, err := BuildStatistics(db)
	if err != nil {
		t.Fatal(err)
	}

	// statisticsYearChartLimit recent years + "Older" + "Unknown".
	if want := statisticsYearChartLimit + 2; len(s.ByYear) != want {
		t.Fatalf("ByYear has %d entries, want %d (Older + %d recent years + Unknown): %+v",
			len(s.ByYear), want, statisticsYearChartLimit, s.ByYear)
	}
	if s.ByYear[0].Label != "Older" {
		t.Fatalf("ByYear[0].Label = %q, want %q (leading, before the recent years)", s.ByYear[0].Label, "Older")
	}
	// The 5 oldest years (i=0..4, sizes 1..5) folded together.
	if s.ByYear[0].Count != 5 {
		t.Errorf("Older.Count = %d, want 5 (the years pushed out of the recent window)", s.ByYear[0].Count)
	}
	if s.ByYear[0].Bytes != 1+2+3+4+5 {
		t.Errorf("Older.Bytes = %d, want %d", s.ByYear[0].Bytes, 1+2+3+4+5)
	}
	// The most recent kept year is startYear+totalYears-1.
	lastRealIdx := len(s.ByYear) - 2 // before "Unknown"
	wantLastYear := startYear + totalYears - 1
	if got := s.ByYear[lastRealIdx].Label; got != strconv.Itoa(wantLastYear) {
		t.Errorf("last real year before Unknown = %q, want %d", got, wantLastYear)
	}
	if got := s.ByYear[len(s.ByYear)-1].Label; got != "Unknown" {
		t.Errorf("last entry = %q, want Unknown (still last, unaffected by the year cap)", got)
	}
}

// statisticsExtensionTableLimit rows, but ExtensionCount (the Overview
// stat) must still report the TRUE distinct-extension total, not the
// capped row count -- the table is trimmed for readability, the summary
// figure isn't.
func TestBuildStatistics_ExtensionTablesCappedButExtensionCountStaysTrue(t *testing.T) {
	db := openTestDB(t)
	for i := 0; i < statisticsExtensionTableLimit+7; i++ {
		ext := string(rune('a' + i))
		mustNoErr(t, db.EnsurePending("h"+ext, int64(i+1), "application/octet-stream", "/lib/f."+ext, nil))
	}

	s, err := BuildStatistics(db)
	if err != nil {
		t.Fatal(err)
	}
	if s.ExtensionCount != statisticsExtensionTableLimit+7 {
		t.Errorf("ExtensionCount = %d, want %d (the true total, uncapped)", s.ExtensionCount, statisticsExtensionTableLimit+7)
	}
	if len(s.ByExtCount) != statisticsExtensionTableLimit {
		t.Errorf("ByExtCount has %d rows, want exactly the cap (%d)", len(s.ByExtCount), statisticsExtensionTableLimit)
	}
	if len(s.ByExtSize) != statisticsExtensionTableLimit {
		t.Errorf("ByExtSize has %d rows, want exactly the cap (%d)", len(s.ByExtSize), statisticsExtensionTableLimit)
	}
}

// TestBuildStatistics_ByKindSplitsPendingFromTotal covers the data behind
// the "By media type" card's pair of bars.
//
// Pending is counted independently rather than derived as total-minus-
// uploaded, because those are not the same thing: a file can also be
// failed_permanent or needs_review, and folding those into "pending"
// would make the bar promise work that will never happen.
func TestBuildStatistics_ByKindSplitsPendingFromTotal(t *testing.T) {
	db := openTestDB(t)
	// Photos: one uploaded, one pending.
	mustNoErr(t, db.EnsurePending("p-done", 1000, "image/jpeg", "/lib/a.jpg", nil))
	mustNoErr(t, db.MarkUploaded("p-done", "m1", ""))
	mustNoErr(t, db.EnsurePending("p-todo", 500, "image/jpeg", "/lib/b.jpg", nil))
	// Videos: one pending, one in the retry bucket (still outstanding
	// work -- every Uploader.Run sweeps it back to pending), and one
	// permanently failed (NOT outstanding, it will never be retried).
	mustNoErr(t, db.EnsurePending("v-todo", 8000, "video/mp4", "/lib/c.mp4", nil))
	mustNoErr(t, db.EnsurePending("v-retry", 2000, "video/mp4", "/lib/d.mp4", nil))
	mustNoErr(t, db.MarkFailed("v-retry", false, "TRANSIENT", "try again"))
	mustNoErr(t, db.EnsurePending("v-dead", 4000, "video/mp4", "/lib/e.mp4", nil))
	mustNoErr(t, db.MarkFailed("v-dead", true, "UNSUPPORTED", "never"))

	s, err := BuildStatistics(db)
	if err != nil {
		t.Fatal(err)
	}
	photos, videos := s.ByKind[0], s.ByKind[1]

	if photos.Count != 2 || photos.Bytes != 1500 {
		t.Errorf("photos total = %d/%d, want 2/1500", photos.Count, photos.Bytes)
	}
	if photos.PendingCount != 1 || photos.PendingBytes != 500 {
		t.Errorf("photos pending = %d/%d, want 1/500", photos.PendingCount, photos.PendingBytes)
	}
	if videos.Count != 3 || videos.Bytes != 14000 {
		t.Errorf("videos total = %d/%d, want 3/14000 (the permanent failure is still part of the library)", videos.Count, videos.Bytes)
	}
	// 8000 pending + 2000 failed_retryable; the 4000 permanent failure is
	// excluded because nothing will ever upload it.
	if videos.PendingCount != 2 || videos.PendingBytes != 10000 {
		t.Errorf("videos pending = %d/%d, want 2/10000 (pending + failed_retryable, NOT failed_permanent)",
			videos.PendingCount, videos.PendingBytes)
	}
}
