package engine

import (
	"fmt"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// ActivityGranularity selects how BuildUploadActivity buckets the upload
// history. Yearly is deliberately absent: with a real library it would be
// a single bar for a very long time, and by the time it wasn't, monthly
// already tells the same story at higher resolution -- a direct call,
// made against the real numbers ("yearly - i think it's too much, no ?"),
// not an omission.
type ActivityGranularity string

const (
	ActivityDaily   ActivityGranularity = "daily"
	ActivityWeekly  ActivityGranularity = "weekly"
	ActivityMonthly ActivityGranularity = "monthly"
)

// How many buckets each granularity looks back over. Daily is 30 per the
// original ask ("last 30 days is sufficient"); the other two are sized so
// each chart holds a comparable number of bars, which is what keeps them
// readable at one screen width.
const (
	activityDailyBuckets   = 30
	activityWeeklyBuckets  = 12
	activityMonthlyBuckets = 12
)

// ActivityBucket is one bar: how many files were uploaded in that span,
// and how many bytes they came to.
type ActivityBucket struct {
	Label string
	Start time.Time
	Files int
	Bytes int64
}

// BuildUploadActivity groups real uploads into a fixed window of
// contiguous buckets ending at now, for the Statistics page's "uploaded
// over time" charts.
//
// Two deliberate choices worth knowing about:
//
// EMPTY BUCKETS ARE INCLUDED. A day nothing was uploaded renders as a
// zero bar rather than being dropped, because dropping it silently
// rewrites the x-axis: ten scattered days would otherwise look like ten
// consecutive ones, and a gap in activity -- the single most interesting
// thing a chart like this can show -- would be invisible.
//
// BUCKETS ARE LOCAL-TIME, not Pacific. This deliberately differs from the
// "Uploaded today" card, which is tied to the quota's own midnight
// Pacific reset (see quota.PacificMidnightEpoch) because it answers a
// quota question. This chart answers an activity question -- "what did I
// upload on Tuesday" -- so it uses the days the user actually lived
// through. The two figures can therefore disagree slightly near midnight;
// that's correct, they're answering different questions.
//
// mark-synced rows never appear here: statedb.SuccessfulUploadTimestamps
// filters on google_media_item_id IS NOT NULL, so files that were only
// ever recorded as already-backed-up (no bytes ever sent) can't inflate
// a throughput chart. Same distinction LedgerSummary already draws.
func BuildUploadActivity(db *statedb.DB, g ActivityGranularity, now time.Time) ([]ActivityBucket, error) {
	uploads, err := db.SuccessfulUploadTimestamps()
	if err != nil {
		return nil, err
	}
	return bucketUploads(uploads, g, now), nil
}

// bucketUploads is BuildUploadActivity minus the database read -- every
// decision that's actually worth testing (window anchoring, empty
// buckets, calendar boundaries, local-time conversion) lives here, so it
// can be exercised with synthetic timestamps. The ledger has no way to
// backdate an uploaded_at, so testing this through the DB would only ever
// reach "everything landed in today's bucket".
func bucketUploads(uploads []statedb.UploadedAtSize, g ActivityGranularity, now time.Time) []ActivityBucket {
	count := activityBucketCount(g)
	// Walk back from the CURRENT bucket so the window always ends "now"
	// even when nothing has been uploaded recently -- a trailing run of
	// empty bars is itself the honest answer to "what did I upload
	// lately", and anchoring on the newest DATA instead would hide it.
	start := bucketStart(now, g)
	for i := 1; i < count; i++ {
		start = bucketStart(start.AddDate(0, 0, -1), g)
	}

	buckets := make([]ActivityBucket, 0, count)
	at := start
	for i := 0; i < count; i++ {
		buckets = append(buckets, ActivityBucket{Label: bucketLabel(at, g), Start: at})
		at = nextBucket(at, g)
	}
	// end is the exclusive upper edge of the last bucket.
	end := at

	for _, u := range uploads {
		t := time.Unix(int64(u.At), 0).Local()
		if t.Before(start) || !t.Before(end) {
			continue
		}
		idx := bucketIndex(buckets, t)
		if idx < 0 {
			continue
		}
		buckets[idx].Files++
		buckets[idx].Bytes += u.Size
	}
	return buckets
}

func activityBucketCount(g ActivityGranularity) int {
	switch g {
	case ActivityWeekly:
		return activityWeeklyBuckets
	case ActivityMonthly:
		return activityMonthlyBuckets
	default:
		return activityDailyBuckets
	}
}

// bucketStart snaps a time down to the start of its own bucket: midnight
// for a day, the preceding Monday for a week, the 1st for a month.
func bucketStart(t time.Time, g ActivityGranularity) time.Time {
	y, m, d := t.Date()
	loc := t.Location()
	switch g {
	case ActivityWeekly:
		day := time.Date(y, m, d, 0, 0, 0, 0, loc)
		// Go's Weekday() puts Sunday at 0; shift so weeks start Monday.
		offset := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -offset)
	case ActivityMonthly:
		return time.Date(y, m, 1, 0, 0, 0, 0, loc)
	default:
		return time.Date(y, m, d, 0, 0, 0, 0, loc)
	}
}

// nextBucket steps to the start of the following bucket. Calendar-aware
// (AddDate, not a fixed duration) so a month step lands on the 1st
// regardless of month length, and a day step stays correct across a
// daylight-saving change -- adding a flat 24h would drift by an hour and
// eventually mis-bucket a whole day.
func nextBucket(t time.Time, g ActivityGranularity) time.Time {
	switch g {
	case ActivityWeekly:
		return t.AddDate(0, 0, 7)
	case ActivityMonthly:
		return t.AddDate(0, 1, 0)
	default:
		return t.AddDate(0, 0, 1)
	}
}

func bucketLabel(t time.Time, g ActivityGranularity) string {
	switch g {
	case ActivityWeekly:
		return t.Format("Jan 2")
	case ActivityMonthly:
		return t.Format("Jan 2006")
	default:
		return t.Format("Jan 2")
	}
}

// bucketIndex finds the bucket a timestamp belongs in by scanning from
// the end. Deliberately not index arithmetic on a fixed bucket width:
// months have different lengths and DST makes some days 23 or 25 hours
// long, so "(t - start) / width" would be subtly wrong at exactly the
// boundaries this chart is meant to show.
func bucketIndex(buckets []ActivityBucket, t time.Time) int {
	for i := len(buckets) - 1; i >= 0; i-- {
		if !t.Before(buckets[i].Start) {
			return i
		}
	}
	return -1
}

// ParseActivityGranularity maps a query-string value to a granularity,
// falling back to daily for anything unrecognized (including empty).
func ParseActivityGranularity(s string) ActivityGranularity {
	switch ActivityGranularity(s) {
	case ActivityWeekly:
		return ActivityWeekly
	case ActivityMonthly:
		return ActivityMonthly
	default:
		return ActivityDaily
	}
}

// ActivityWindowNote describes the rendered window in words, for the
// chart's own caption.
func ActivityWindowNote(g ActivityGranularity) string {
	switch g {
	case ActivityWeekly:
		return fmt.Sprintf("Last %d weeks, by week starting Monday.", activityWeeklyBuckets)
	case ActivityMonthly:
		return fmt.Sprintf("Last %d months.", activityMonthlyBuckets)
	default:
		return fmt.Sprintf("Last %d days.", activityDailyBuckets)
	}
}
