package engine

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

// seedDrilldownLibrary builds a ledger that reaches every branch the
// Statistics charts have: more than statisticsYearChartLimit distinct years
// (so an "Older" bar exists), years from the path and from captured_at,
// implausible and missing dates ("Unknown"), several extensions including
// none, photos, videos and other files, and every status a chart counts --
// including mark-synced rows, which are 'uploaded' but never moved a byte.
func seedDrilldownLibrary(t *testing.T, db *statedb.DB) {
	t.Helper()
	n := 0
	add := func(path string, captured *float64, status string) {
		n++
		sha := fmt.Sprintf("h%04d", n)
		if status == "marked" {
			mustNoErr(t, db.EnsureMarkedSynced(sha, int64(n), "image/jpeg", path, captured))
			return
		}
		mustNoErr(t, db.EnsurePending(sha, int64(n), "image/jpeg", path, captured))
		switch status {
		case "uploaded":
			mustNoErr(t, db.MarkUploaded(sha, "media-"+sha, ""))
		case "failed_retryable":
			mustNoErr(t, db.MarkFailed(sha, false, "throttle", "429"))
		case "failed_permanent":
			mustNoErr(t, db.MarkFailed(sha, true, "unsupported", "nope"))
		}
	}
	statuses := []string{"uploaded", "pending", "failed_retryable", "failed_permanent", "marked"}
	exts := []string{"jpg", "png", "mp4", "mov", "bmp", "heic"}
	// 30 path-dated years, 1990..2019 -- five more than the chart shows.
	for y := 1990; y < 2020; y++ {
		for i := 0; i < 2; i++ {
			add(fmt.Sprintf("/lib/%d/img%c.%s", y, 'a'+i, exts[(y+i)%len(exts)]), nil, statuses[(y+i)%len(statuses)])
		}
	}
	// Years that come from captured_at: the paths carry no digits a path
	// date could be read from.
	for i, y := range []int{2021, 2022, 2022, 2023} {
		c := float64(time.Date(y, 6, 1, 12, 0, 0, 0, time.UTC).Unix())
		add(fmt.Sprintf("/lib/misc/cap%c.%s", 'a'+i, exts[i]), &c, statuses[i%len(statuses)])
	}
	// Unknown: an implausible EXIF year, one in the far future, and none.
	bad := float64(time.Date(1850, 1, 1, 0, 0, 0, 0, time.UTC).Unix())
	future := float64(time.Now().AddDate(5, 0, 0).Unix())
	add("/lib/misc/ancient.jpg", &bad, "uploaded")
	add("/lib/misc/future.mp4", &future, "pending")
	add("/lib/misc/undated.png", nil, "failed_retryable")
	add("/lib/misc/noext", nil, "uploaded")
}

func browseTotal(t *testing.T, db *statedb.DB, kind FileListKind, f BrowseFilter) int {
	t.Helper()
	res, err := BrowseFiles(db, BrowseQuery{Kind: kind, Filter: f, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	return res.Total
}

// Issue #53. The rule a drill-down exists to keep: the list a chart element
// opens contains exactly the files it counted -- for every bar of every
// chart, not a sample.
func TestDrilldown_EveryStatisticsBucketListsExactlyItsCount(t *testing.T) {
	db := openTestDB(t)
	seedDrilldownLibrary(t, db)
	s, err := BuildStatistics(db)
	if err != nil {
		t.Fatal(err)
	}

	if len(s.ByYear) == 0 || s.ByYear[0].Label != "Older" {
		t.Fatalf("the seed must produce an Older bar to test it; ByYear = %+v", s.ByYear)
	}
	sawUnknown := false
	for _, y := range s.ByYear {
		year := y.Label
		if year == "Older" || year == "Unknown" {
			year = strings.ToLower(year)
			sawUnknown = sawUnknown || year == "unknown"
		}
		if got := browseTotal(t, db, FileListAll, BrowseFilter{Year: year}); got != y.Count {
			t.Errorf("capture year %s: the bar counts %d, its list has %d", y.Label, y.Count, got)
		}
	}
	if !sawUnknown {
		t.Error("the seed must produce an Unknown bar to test it")
	}

	for _, e := range s.ByExtCount {
		if got := browseTotal(t, db, FileListAll, BrowseFilter{Ext: e.Label}); got != e.Count {
			t.Errorf("extension %s: the row counts %d, its list has %d", e.Label, e.Count, got)
		}
	}

	kinds := map[string]string{"Photos": "photo", "Videos": "video", "Other": "other"}
	for _, k := range s.ByKind {
		f := BrowseFilter{Kind: kinds[k.Label]}
		if got := browseTotal(t, db, FileListAll, f); got != k.Count {
			t.Errorf("%s: %d in total, its list has %d", k.Label, k.Count, got)
		}
		if got := browseTotal(t, db, FileListUploaded, f); got != k.UploadedCount {
			t.Errorf("%s uploaded segment: %d, its list has %d", k.Label, k.UploadedCount, got)
		}
		f.Outstanding = true
		if got := browseTotal(t, db, FileListAll, f); got != k.PendingCount {
			t.Errorf("%s pending segment: %d, its list has %d", k.Label, k.PendingCount, got)
		}
	}

	for _, g := range []ActivityGranularity{ActivityDaily, ActivityWeekly, ActivityMonthly} {
		buckets, err := BuildUploadActivity(db, g, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range buckets {
			if got := browseTotal(t, db, FileListUploaded, BrowseFilter{UploadedOn: b.Start, Span: g}); got != b.Files {
				t.Errorf("%s bar %s: counts %d uploads, its list has %d", g, b.Label, b.Files, got)
			}
		}
	}
}

// Every upload in a real ledger lands at a different time; the one above
// lands "now". This drives the activity chart's own bucketing with
// synthetic upload times -- including the edges, where a bar and its list
// are most likely to disagree -- and holds each bucket's list to it.
func TestDrilldown_ActivityBucketsMatchAtTheirEdges(t *testing.T) {
	now := time.Date(2026, 3, 31, 15, 0, 0, 0, time.Local)
	var rows []statedb.Upload
	upload := func(at time.Time, real bool) {
		u := statedb.Upload{Status: "uploaded", UploadedAt: sql.NullFloat64{Float64: float64(at.UnixNano()) / 1e9, Valid: true}}
		if real {
			u.GoogleMediaItemID = sql.NullString{String: "m", Valid: true}
		}
		rows = append(rows, u)
	}
	for d := 0; d < 400; d += 3 {
		day := time.Date(2026, 3, 31, 0, 0, 0, 0, time.Local).AddDate(0, 0, -d)
		upload(day, true)                                   // exactly midnight
		upload(day.Add(-500*time.Millisecond), true)        // the last half-second of the day before
		upload(day.Add(12*time.Hour), d%2 == 0)             // midday; every other one marked-synced
		upload(day.Add(24*time.Hour-time.Nanosecond), true) // the last instant of the day
	}

	var stamps []statedb.UploadedAtSize
	for _, u := range rows {
		if u.RealUpload() { // proven equal to the chart's SQL in statedb
			stamps = append(stamps, statedb.UploadedAtSize{At: u.UploadedAt.Float64, Size: 1})
		}
	}
	for _, g := range []ActivityGranularity{ActivityDaily, ActivityWeekly, ActivityMonthly} {
		for _, b := range bucketUploads(stamps, g, now) {
			from, to := ActivityRange(b.Start, g)
			got := 0
			for _, u := range rows {
				if InActivityRange(u, from, to) {
					got++
				}
			}
			if got != b.Files {
				t.Errorf("%s bar %s: counts %d, its list has %d", g, b.Label, b.Files, got)
			}
		}
	}
}

// Links are built from Values and read back with ParseBrowseFilter; a
// condition lost in the round trip would silently widen the list.
func TestBrowseFilter_RoundTripsThroughAURL(t *testing.T) {
	want := BrowseFilter{Kind: "video", Ext: "mp4", Year: "older", Outstanding: true,
		UploadedOn: time.Date(2026, 10, 21, 0, 0, 0, 0, time.Local), Span: ActivityWeekly}
	got := ParseBrowseFilter(want.Values())
	if !got.UploadedOn.Equal(want.UploadedOn) || got.Kind != want.Kind || got.Ext != want.Ext ||
		got.Year != want.Year || got.Outstanding != want.Outstanding || got.Span != want.Span {
		t.Errorf("round trip: got %+v, want %+v", got, want)
	}
}

// Every value arrives from a URL. Anything outside the closed sets is
// dropped, never passed on.
func TestParseBrowseFilter_DropsWhatItDoesNotRecognize(t *testing.T) {
	v := url.Values{
		"kind": {"photo' OR 1=1"}, "ext": {"../../etc"}, "year": {"20x2"},
		"st": {"everything"}, "up": {"yesterday"},
	}
	if f := ParseBrowseFilter(v); f.Active() {
		t.Errorf("ParseBrowseFilter kept %+v from values it should not recognize", f)
	}
}
