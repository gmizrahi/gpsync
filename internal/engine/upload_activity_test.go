package engine

import (
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// at builds an upload record at a local wall-clock time, which is the
// only thing these tests care about -- BuildUploadActivity buckets by
// LOCAL calendar day/week/month, so the fixtures have to be expressed the
// same way to mean anything.
func at(y int, m time.Month, d, h int, size int64) statedb.UploadedAtSize {
	return statedb.UploadedAtSize{
		At:   float64(time.Date(y, m, d, h, 0, 0, 0, time.Local).Unix()),
		Size: size,
	}
}

func findBucket(t *testing.T, buckets []ActivityBucket, label string) ActivityBucket {
	t.Helper()
	for _, b := range buckets {
		if b.Label == label {
			return b
		}
	}
	t.Fatalf("no bucket labelled %q in %d buckets (%q..%q)", label, len(buckets), buckets[0].Label, buckets[len(buckets)-1].Label)
	return ActivityBucket{}
}

func TestBucketUploads_Daily_GroupsByLocalDayAndKeepsEmptyDays(t *testing.T) {
	now := time.Date(2026, time.September, 8, 14, 0, 0, 0, time.Local)
	uploads := []statedb.UploadedAtSize{
		at(2026, time.September, 8, 9, 100),
		at(2026, time.September, 8, 11, 200),
		// Nothing at all on the 7th -- it must still get a (zero) bucket.
		at(2026, time.September, 6, 23, 500),
	}

	buckets := bucketUploads(uploads, ActivityDaily, now)

	if len(buckets) != activityDailyBuckets {
		t.Fatalf("len(buckets) = %d, want %d", len(buckets), activityDailyBuckets)
	}
	// The window must END at today, so the last bar is always "now".
	if last := buckets[len(buckets)-1]; last.Label != "Sep 8" {
		t.Errorf("last bucket = %q, want the current day (Sep 8)", last.Label)
	}
	if b := findBucket(t, buckets, "Sep 8"); b.Files != 2 || b.Bytes != 300 {
		t.Errorf("Sep 8 = %d files/%d bytes, want 2/300", b.Files, b.Bytes)
	}
	if b := findBucket(t, buckets, "Sep 6"); b.Files != 1 || b.Bytes != 500 {
		t.Errorf("Sep 6 = %d files/%d bytes, want 1/500", b.Files, b.Bytes)
	}
	// The whole point of keeping empty buckets: a day with no uploads is
	// a visible zero, not a bar quietly removed from the axis.
	if b := findBucket(t, buckets, "Sep 7"); b.Files != 0 || b.Bytes != 0 {
		t.Errorf("Sep 7 = %d files/%d bytes, want an empty bucket to still be present", b.Files, b.Bytes)
	}
}

func TestBucketUploads_DropsAnythingOutsideTheWindow(t *testing.T) {
	now := time.Date(2026, time.September, 8, 14, 0, 0, 0, time.Local)
	uploads := []statedb.UploadedAtSize{
		at(2026, time.September, 8, 9, 100), // in
		at(2025, time.January, 1, 9, 999),   // far too old
		// Later today than "now" -- a clock skew or a row written moments
		// ahead must not fall outside the last bucket.
		at(2026, time.September, 8, 23, 50),
	}

	buckets := bucketUploads(uploads, ActivityDaily, now)

	var files int
	var bytes int64
	for _, b := range buckets {
		files += b.Files
		bytes += b.Bytes
	}
	if files != 2 || bytes != 150 {
		t.Errorf("totals = %d files/%d bytes, want 2/150 (the 2025 row is outside the 30-day window; the later-today row is not)", files, bytes)
	}
}

func TestBucketUploads_Weekly_StartsWeeksOnMondayAndSpansTheWholeWeek(t *testing.T) {
	// 2026-09-08 is a Tuesday, so its week starts Monday 2026-09-07.
	now := time.Date(2026, time.September, 8, 14, 0, 0, 0, time.Local)
	uploads := []statedb.UploadedAtSize{
		at(2026, time.September, 7, 1, 10),  // Monday, same week
		at(2026, time.September, 8, 23, 20), // Tuesday, same week
		at(2026, time.September, 6, 12, 30), // Sunday -- the PREVIOUS week
	}

	buckets := bucketUploads(uploads, ActivityWeekly, now)

	if len(buckets) != activityWeeklyBuckets {
		t.Fatalf("len(buckets) = %d, want %d", len(buckets), activityWeeklyBuckets)
	}
	last := buckets[len(buckets)-1]
	if last.Label != "Sep 7" {
		t.Fatalf("last bucket = %q, want the Monday of the current week (Sep 7)", last.Label)
	}
	if last.Files != 2 || last.Bytes != 30 {
		t.Errorf("current week = %d files/%d bytes, want 2/30", last.Files, last.Bytes)
	}
	// Sunday belongs to the week BEFORE, not the current one -- Go's own
	// Weekday() would put it at the start of a Sunday-based week.
	prev := buckets[len(buckets)-2]
	if prev.Files != 1 || prev.Bytes != 30 {
		t.Errorf("previous week = %d files/%d bytes, want 1/30 (Sunday belongs to the earlier week)", prev.Files, prev.Bytes)
	}
}

func TestBucketUploads_Monthly_GroupsByCalendarMonth(t *testing.T) {
	now := time.Date(2026, time.September, 8, 14, 0, 0, 0, time.Local)
	uploads := []statedb.UploadedAtSize{
		at(2026, time.September, 1, 0, 1),
		at(2026, time.September, 30, 23, 2),
		at(2026, time.August, 31, 23, 4),
	}

	buckets := bucketUploads(uploads, ActivityMonthly, now)

	if len(buckets) != activityMonthlyBuckets {
		t.Fatalf("len(buckets) = %d, want %d", len(buckets), activityMonthlyBuckets)
	}
	if b := findBucket(t, buckets, "Sep 2026"); b.Files != 2 || b.Bytes != 3 {
		t.Errorf("Sep 2026 = %d files/%d bytes, want 2/3", b.Files, b.Bytes)
	}
	// Aug 31 23:00 is the last hour of August; a fixed 30-day step would
	// drag it into September.
	if b := findBucket(t, buckets, "Aug 2026"); b.Files != 1 || b.Bytes != 4 {
		t.Errorf("Aug 2026 = %d files/%d bytes, want 1/4", b.Files, b.Bytes)
	}
}

func TestBucketUploads_MonthlyWindowWalksBackByCalendarMonths(t *testing.T) {
	// Anchored on the 31st on purpose: stepping back a month from a long
	// month is where naive date arithmetic overflows (Go normalizes
	// Mar 31 minus one month to Mar 3), which would silently skip a
	// month and mislabel every bar after it.
	now := time.Date(2026, time.March, 31, 12, 0, 0, 0, time.Local)

	buckets := bucketUploads(nil, ActivityMonthly, now)

	if got := buckets[len(buckets)-1].Label; got != "Mar 2026" {
		t.Errorf("last bucket = %q, want Mar 2026", got)
	}
	// 12 buckets ending in March 2026 means the first is April 2025.
	if got := buckets[0].Label; got != "Apr 2025" {
		t.Errorf("first bucket = %q, want Apr 2025", got)
	}
	for i := 1; i < len(buckets); i++ {
		if !buckets[i].Start.After(buckets[i-1].Start) {
			t.Fatalf("buckets must be strictly ascending; %q (%v) does not follow %q (%v)",
				buckets[i].Label, buckets[i].Start, buckets[i-1].Label, buckets[i-1].Start)
		}
		if d := buckets[i].Start.Day(); d != 1 {
			t.Errorf("monthly bucket %q starts on day %d, want the 1st", buckets[i].Label, d)
		}
	}
}

func TestBucketUploads_NoUploads_StillReturnsAFullEmptyWindow(t *testing.T) {
	now := time.Date(2026, time.September, 8, 14, 0, 0, 0, time.Local)

	buckets := bucketUploads(nil, ActivityDaily, now)

	if len(buckets) != activityDailyBuckets {
		t.Fatalf("len(buckets) = %d, want a full window of %d even with no data", len(buckets), activityDailyBuckets)
	}
	for _, b := range buckets {
		if b.Files != 0 || b.Bytes != 0 {
			t.Fatalf("bucket %q = %d files/%d bytes, want all zero", b.Label, b.Files, b.Bytes)
		}
	}
}

func TestParseActivityGranularity(t *testing.T) {
	cases := map[string]ActivityGranularity{
		"daily":   ActivityDaily,
		"weekly":  ActivityWeekly,
		"monthly": ActivityMonthly,
		"":        ActivityDaily,
		"yearly":  ActivityDaily, // deliberately unsupported, see the type's doc comment
		"garbage": ActivityDaily,
	}
	for in, want := range cases {
		if got := ParseActivityGranularity(in); got != want {
			t.Errorf("ParseActivityGranularity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildUploadActivity_ReadsTheLedgerAndExcludesMarkSynced(t *testing.T) {
	db := openTestDB(t)

	// A real upload (has a google media item id) and a mark-synced row
	// (no id, no bytes ever sent) -- only the former is throughput.
	mustNoErr(t, db.EnsurePending("h-real", 1234, "image/jpeg", "/lib/real.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h-real", "media-real", ""))
	mustNoErr(t, db.EnsureMarkedSynced("h-synced", 9999, "image/jpeg", "/lib/synced.jpg", nil))

	buckets, err := BuildUploadActivity(db, ActivityDaily, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	var files int
	var bytes int64
	for _, b := range buckets {
		files += b.Files
		bytes += b.Bytes
	}
	if files != 1 || bytes != 1234 {
		t.Errorf("totals = %d files/%d bytes, want 1/1234 -- a mark-synced row moved no bytes and must not appear in a throughput chart", files, bytes)
	}
}
