package engine

import (
	"sort"
	"strconv"
	"time"

	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// Statistics is the full picture behind gpsync-tray's Statistics page -- a
// real request, after seeing a design mockup with sample data: "the
// statistics is nice, if we can add more data and visualization would be
// nice." Built from statedb.AllSizes, which covers every tracked hash
// regardless of status (pending, uploaded, failed) -- this describes the
// whole library, not just what's left to upload.
type Statistics struct {
	TotalFiles     int
	TotalBytes     int64
	AvgBytes       float64
	ExtensionCount int

	// ByKind is always exactly 3 entries, in this fixed order (Photos,
	// Videos, Other) -- not sorted by size, since a stable category order
	// reads better than one that reshuffles as the library changes.
	ByKind []KindStat

	// ByExtCount/ByExtSize are the SAME data, sorted two different ways --
	// matching the two-table layout the approved mockup used ("by
	// extension - count" / "by extension - total size").
	ByExtCount []CountStat
	ByExtSize  []CountStat

	// ByYear is sorted chronologically ascending, capped to the most
	// recent statisticsYearChartLimit years -- anything older is folded
	// into one leading "Older" bucket -- with an "Unknown" bucket (no
	// readable/plausible capture date) last regardless of where it would
	// otherwise sort. A capped chart with one "Older" bucket was chosen
	// over a scrollable one: the whole point is to take in the shape of a
	// library at a glance.
	ByYear []CountStat
}

// CountStat is one labeled count/bytes bucket -- a media kind, an
// extension, or a capture year, depending on which Statistics field it's
// in.
type CountStat struct {
	Label string
	Count int
	Bytes int64
}

// statisticsExtensionTableLimit caps each "by extension" table -- a real
// report: "extensions - top 10 or even top 5 is perhaps sufficient?" A
// library with many rare/one-off extensions would otherwise print a
// long tail of single-digit rows nobody's actually looking for.
const statisticsExtensionTableLimit = 10

// statisticsMinPlausibleYear is the floor for a capture year to be
// plotted on its own -- anything below it (or more than a year in the
// future, allowing for camera clock skew) folds into the "Unknown"
// bucket instead. Cameras and apps do sometimes write garbage EXIF
// capture dates (not a parsing bug here -- the raw value really is what
// is stored in captured_at), and a year like 4500 on the chart is the
// visible result. 1900 is a deliberately generous floor -- it
// accepts any genuinely old scanned/digitized photo while still
// catching classic epoch/zero-date artifacts (year 0, 1, 1904's HFS
// epoch, etc.) that would otherwise get their own misleading bar.
const statisticsMinPlausibleYear = 1900

// statisticsYearChartLimit caps the year chart to the most recent N
// individual years -- see ByYear's own doc comment for why.
const statisticsYearChartLimit = 25

// yearFromPath is a thin wrapper over scanner.DateFromPath -- the SAME
// path-date heuristic scanner.captureDate() now tries before falling
// back to filesystem mtime (see its own doc comment for the root cause:
// mtime drifts when a later copy or migration touches a file). Checked
// BEFORE
// captured_at in the year chart's own aggregation below, same reasoning
// as scanner.captureDate's priority: this doesn't rely on `gpsync
// fix-dates` having been run against the already-scanned ledger, so the
// chart is accurate immediately regardless. Doesn't affect anything
// stored in the ledger itself (captured_at, Browse's Captured column)
// -- only this page's own year-chart display; `gpsync
// fix-dates` is what fixes the ledger for everything else that reads
// captured_at.
func yearFromPath(path string) (int, bool) {
	t, ok := scanner.DateFromPath(path)
	if !ok {
		return 0, false
	}
	return t.Year(), true
}

// KindStat is one media-type row on the Statistics page. Unlike
// CountStat it carries a PENDING figure alongside the total, because the
// card shows both as a pair of bars. Total minus pending is not the same
// as
// "uploaded" (a file can be failed_permanent or needs_review), so the two
// are counted independently rather than derived from each other.
type KindStat struct {
	Label string
	Count int
	Bytes int64
	// UploadedCount/UploadedBytes and PendingCount/PendingBytes are the
	// two SEGMENTS of that category's bar. They do not necessarily sum to
	// the total, and that gap is deliberate: a file can also be
	// failed_permanent or needs_review, which is neither done nor
	// outstanding. Rendering it as unfilled track is honest; folding it
	// into either segment would not be.
	UploadedCount int
	UploadedBytes int64
	PendingCount  int
	PendingBytes  int64
}

// BuildStatistics aggregates statedb.AllSizes into Statistics. Kind is
// derived from each path's extension (extensions.KindOf); a file whose
// extension isn't a recognized supported photo/video type -- including
// anything genuinely unsupported/unknown to Google Photos -- falls into
// the "Other" bucket rather than being guessed at.
func BuildStatistics(db *statedb.DB) (Statistics, error) {
	rows, err := db.AllSizes()
	if err != nil {
		return Statistics{}, err
	}

	var s Statistics
	kindAgg := map[extensions.Kind]*KindStat{
		extensions.KindPhoto:   {Label: "Photos"},
		extensions.KindVideo:   {Label: "Videos"},
		extensions.KindUnknown: {Label: "Other"},
	}
	extAgg := map[string]*CountStat{}
	yearAgg := map[string]*CountStat{}

	for _, r := range rows {
		s.TotalFiles++
		s.TotalBytes += r.Size

		kind := extensions.KindOf(r.Path)
		kindAgg[kind].Count++
		kindAgg[kind].Bytes += r.Size
		// failed_retryable counts as outstanding for the same reason
		// BuildForecast treats it that way: every Uploader.Run sweeps it
		// back to pending, so it is work still ahead, not work written off.
		switch r.Status {
		case "uploaded":
			kindAgg[kind].UploadedCount++
			kindAgg[kind].UploadedBytes += r.Size
		case "pending", "failed_retryable":
			// failed_retryable counts as outstanding for the same reason
			// BuildForecast treats it that way: every Uploader.Run sweeps
			// it back to pending, so it is work still ahead.
			kindAgg[kind].PendingCount++
			kindAgg[kind].PendingBytes += r.Size
		}

		ext := extensions.ExtOf(r.Path)
		if extAgg[ext] == nil {
			extAgg[ext] = &CountStat{Label: ext}
		}
		extAgg[ext].Count++
		extAgg[ext].Bytes += r.Size

		year := "Unknown"
		if y, ok := yearFromPath(r.Path); ok {
			year = strconv.Itoa(y)
		} else if r.CapturedAt.Valid {
			if y := time.Unix(int64(r.CapturedAt.Float64), 0).UTC().Year(); y >= statisticsMinPlausibleYear && y <= time.Now().Year()+1 {
				year = strconv.Itoa(y)
			}
			// Outside that range: some cameras and apps write garbage
			// EXIF capture dates (not a parsing bug here; the raw value
			// really is what is stored in captured_at), which is how a
			// year like 4500 reaches the chart. A
			// file like that has no MEANINGFUL capture year, so it
			// folds into the same "Unknown" bucket as a missing date
			// rather than plotting a bar that makes every real year
			// look flat by comparison (the chart's own scale is
			// relative to whichever bucket is largest).
		}
		if yearAgg[year] == nil {
			yearAgg[year] = &CountStat{Label: year}
		}
		yearAgg[year].Count++
		yearAgg[year].Bytes += r.Size
	}

	if s.TotalFiles > 0 {
		s.AvgBytes = float64(s.TotalBytes) / float64(s.TotalFiles)
	}
	s.ExtensionCount = len(extAgg)

	s.ByKind = []KindStat{*kindAgg[extensions.KindPhoto], *kindAgg[extensions.KindVideo], *kindAgg[extensions.KindUnknown]}

	s.ByExtCount = flattenStats(extAgg)
	sort.Slice(s.ByExtCount, func(i, j int) bool {
		if s.ByExtCount[i].Count != s.ByExtCount[j].Count {
			return s.ByExtCount[i].Count > s.ByExtCount[j].Count
		}
		return s.ByExtCount[i].Label < s.ByExtCount[j].Label
	})
	s.ByExtCount = capStats(s.ByExtCount, statisticsExtensionTableLimit)
	s.ByExtSize = flattenStats(extAgg)
	sort.Slice(s.ByExtSize, func(i, j int) bool {
		if s.ByExtSize[i].Bytes != s.ByExtSize[j].Bytes {
			return s.ByExtSize[i].Bytes > s.ByExtSize[j].Bytes
		}
		return s.ByExtSize[i].Label < s.ByExtSize[j].Label
	})
	s.ByExtSize = capStats(s.ByExtSize, statisticsExtensionTableLimit)

	s.ByYear = flattenStats(yearAgg)
	sort.Slice(s.ByYear, func(i, j int) bool {
		if s.ByYear[i].Label == "Unknown" {
			return false
		}
		if s.ByYear[j].Label == "Unknown" {
			return true
		}
		return s.ByYear[i].Label < s.ByYear[j].Label
	})
	s.ByYear = capYearsToRecentWindow(s.ByYear, statisticsYearChartLimit)

	return s, nil
}

func flattenStats(m map[string]*CountStat) []CountStat {
	out := make([]CountStat, 0, len(m))
	for _, v := range m {
		out = append(out, *v)
	}
	return out
}

// capStats truncates an already-sorted slice to at most limit entries --
// s.ExtensionCount (computed from the full, uncapped map) is still the
// true total, this only bounds how many rows the tables themselves show.
func capStats(s []CountStat, limit int) []CountStat {
	if len(s) > limit {
		return s[:limit]
	}
	return s
}

// capYearsToRecentWindow keeps at most limit individual real years (the
// MOST RECENT ones -- years is chronological ascending on input), folding
// anything older into one leading "Older" bucket. years may optionally
// end with an "Unknown" entry, which is always preserved as-is and
// excluded from the count/fold.
func capYearsToRecentWindow(years []CountStat, limit int) []CountStat {
	real := years
	var unknown *CountStat
	if n := len(years); n > 0 && years[n-1].Label == "Unknown" {
		real = years[:n-1]
		u := years[n-1]
		unknown = &u
	}
	if len(real) <= limit {
		return years
	}
	overflow := len(real) - limit
	older := CountStat{Label: "Older"}
	for _, y := range real[:overflow] {
		older.Count += y.Count
		older.Bytes += y.Bytes
	}
	out := append([]CountStat{older}, real[overflow:]...)
	if unknown != nil {
		out = append(out, *unknown)
	}
	return out
}
