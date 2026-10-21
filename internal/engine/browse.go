package engine

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// FileListKind selects which subset of the ledger BrowseFiles returns --
// the same three buckets `gpsync info`'s summary already counts, just with
// the actual rows behind each count now reachable from the dashboard
// instead of only a number.
type FileListKind string

const (
	FileListPending         FileListKind = "pending"
	FileListFailedRetryable FileListKind = "failed_retryable"
	FileListFailedPermanent FileListKind = "failed_permanent"
	// FileListUploaded/NeedsReview/Ignored close the gap `whatis` and a real
	// Field Report both flagged: before these existed, an uploaded, needs_review,
	// or ignored row had no way to be SEEN in the dashboard at all -- only
	// counted in a Statistics total. Backed by statedb.ListByStatus, not
	// ListPending/ListFailures, since neither of those can return them.
	FileListUploaded    FileListKind = "uploaded"
	FileListNeedsReview FileListKind = "needs_review"
	FileListIgnored     FileListKind = "ignored"
	// FileListAll is every row whatever its status -- what the Statistics
	// page's year, extension and media-type figures describe, so a
	// drill-down from one of them can list exactly what it counted.
	FileListAll FileListKind = "all"
)

// SortKey selects which column BrowseFiles sorts by, before paging.
// SortKeyPath (the zero value) matches
// ListPending/ListFailures' own default ORDER BY, so an unspecified sort
// behaves exactly as it did before sorting existed.
type SortKey string

const (
	SortKeyPath     SortKey = "path"
	SortKeyName     SortKey = "name"
	SortKeySize     SortKey = "size"
	SortKeyType     SortKey = "type"
	SortKeyCaptured SortKey = "captured"
	SortKeyAttempts SortKey = "attempts"
	// SortKeyWhen sorts by RowTime -- Browse's per-tab timestamp column.
	SortKeyWhen SortKey = "when"
)

// RowTime is the moment Browse's timestamp column shows for a row: when it
// was uploaded on the Uploaded tab, otherwise when it entered its current
// status (or, for a failure, last failed). ok is false when the ledger has
// no honest value -- a row from before status_changed_at was recorded --
// and the caller must show it as unknown, never as some default date.
//
// Uploaded rows read uploaded_at rather than status_changed_at: it is the
// authoritative upload time and, unlike status_changed_at, it is known for
// rows uploaded before that column existed. For a MarkedSynced row it is
// the time of marking; the caller says so.
func RowTime(u statedb.Upload) (unix float64, ok bool) {
	if u.Status == "uploaded" {
		return u.UploadedAt.Float64, u.UploadedAt.Valid
	}
	return u.StatusChangedAt.Float64, u.StatusChangedAt.Valid
}

// BrowseQuery is BrowseFiles' input -- a struct rather than a long
// positional parameter list, since it was already growing one field at a
// time (kind, search, now sort, and paging).
type BrowseQuery struct {
	Kind FileListKind
	// Search is a case-insensitive substring match against either the
	// file's path or its content hash -- the hash side is what makes this
	// `whatis` in dashboard form: pasting a sha256 (full or partial) finds
	// the same row `gpsync whatis <file>` would, without needing the local
	// file's bytes on hand to re-hash.
	Search   string
	Sort     SortKey
	SortDesc bool
	Offset   int
	Limit    int
	// Filter narrows the list to what one Statistics chart element
	// counted. The zero value filters nothing.
	Filter BrowseFilter
}

// BrowseResult is one page of BrowseFiles' matching rows, plus enough to
// render "showing X-Y of Z" and Prev/Next controls.
type BrowseResult struct {
	Rows   []statedb.Upload
	Total  int // count of every row matching Kind+Search, before paging
	Offset int
}

// BrowseFiles lists rows from one ledger bucket, optionally filtered by a
// path substring, sorted, then returns one page of them. Reuses statedb's
// existing ListPending/ListFailures (the same queries `gpsync pending`/`gpsync
// log` already use) rather than adding new SQL -- filtering/sorting/paging
// all happen in-memory here since none of that ever existed on those
// queries before (a CLI command printing everything to a scrollable
// terminal has no reason for any of it; a browser tab is a different
// story: a real ledger in this project has had 40k+ pending rows at once).
func BrowseFiles(db *statedb.DB, q BrowseQuery) (BrowseResult, error) {
	var all []statedb.Upload
	var err error
	switch q.Kind {
	case FileListPending:
		all, err = db.ListPending()
	case FileListFailedRetryable:
		all, err = filterByStatus(db, "failed_retryable")
	case FileListFailedPermanent:
		all, err = db.ListFailures(true)
	case FileListUploaded:
		all, err = db.ListByStatus("uploaded")
	case FileListNeedsReview:
		all, err = db.ListByStatus("needs_review")
	case FileListIgnored:
		all, err = db.ListByStatus("ignored")
	case FileListAll:
		all, err = db.ListAll()
	default:
		return BrowseResult{}, fmt.Errorf("unknown file list kind %q", q.Kind)
	}
	if err != nil {
		return BrowseResult{}, err
	}

	if q.Filter.Active() {
		match, err := q.Filter.matcher(db, time.Now())
		if err != nil {
			return BrowseResult{}, err
		}
		filtered := all[:0]
		for _, u := range all {
			if match(u) {
				filtered = append(filtered, u)
			}
		}
		all = filtered
	}

	if q.Search != "" {
		needle := strings.ToLower(q.Search)
		filtered := all[:0]
		for _, u := range all {
			if strings.Contains(strings.ToLower(u.FirstSourcePath), needle) || strings.Contains(strings.ToLower(u.SHA256), needle) {
				filtered = append(filtered, u)
			}
		}
		all = filtered
	}

	sortRows(all, q.Sort, q.SortDesc)

	total := len(all)
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := offset + q.Limit
	if end > total {
		end = total
	}
	return BrowseResult{Rows: all[offset:end], Total: total, Offset: offset}, nil
}

// sortRows sorts in place by key, ascending unless desc. Ties (including
// every non-primary field once the primary comparison is equal) always
// fall back to ascending path order, regardless of desc, so paging through
// a tied sort -- e.g. many files with the same size -- stays in a stable,
// predictable order across requests instead of flipping within tie groups
// depending on direction.
func sortRows(rows []statedb.Upload, key SortKey, desc bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch key {
		case SortKeyName:
			an, bn := strings.ToLower(filepath.Base(a.FirstSourcePath)), strings.ToLower(filepath.Base(b.FirstSourcePath))
			if an != bn {
				return lessDir(an < bn, desc)
			}
		case SortKeySize:
			if a.Size != b.Size {
				return lessDir(a.Size < b.Size, desc)
			}
		case SortKeyType:
			if a.MimeType != b.MimeType {
				return lessDir(a.MimeType < b.MimeType, desc)
			}
		case SortKeyAttempts:
			if a.AttemptCount != b.AttemptCount {
				return lessDir(a.AttemptCount < b.AttemptCount, desc)
			}
		case SortKeyWhen:
			// Unknown sorts last in both directions, as with Captured --
			// a missing time must never pass for the oldest one.
			at, aKnown := RowTime(a)
			bt, bKnown := RowTime(b)
			if aKnown != bKnown {
				return aKnown
			}
			if aKnown && at != bt {
				return lessDir(at < bt, desc)
			}
		case SortKeyCaptured:
			// A row with no known capture date sorts last, in BOTH
			// directions -- unlike every other column, direction here
			// never reverses "unknown" to look like "oldest" (1970).
			aKnown, bKnown := a.CapturedAt.Valid, b.CapturedAt.Valid
			if aKnown != bKnown {
				return aKnown
			}
			if aKnown && a.CapturedAt.Float64 != b.CapturedAt.Float64 {
				return lessDir(a.CapturedAt.Float64 < b.CapturedAt.Float64, desc)
			}
		default: // SortKeyPath, or unset -- matches ListPending/ListFailures' own default order.
			return lessDir(a.FirstSourcePath < b.FirstSourcePath, desc)
		}
		return a.FirstSourcePath < b.FirstSourcePath
	})
}

// lessDir flips a normally-ascending strict comparison for a descending
// sort. Only ever called with the result of an already-confirmed `!=`
// comparison, so asc is never the "equal" case -- !asc is exactly "greater
// than", which is correct for "is a before b in descending order".
func lessDir(asc, desc bool) bool {
	if desc {
		return !asc
	}
	return asc
}

// filterByStatus narrows ListFailures' combined failed_retryable +
// failed_permanent result down to just one status -- there's no dedicated
// statedb query for "retryable only", and adding one for a single caller
// wasn't worth it next to filtering the (already small, by definition:
// gpsync's whole design keeps permanent failures rare) combined list.
func filterByStatus(db *statedb.DB, status string) ([]statedb.Upload, error) {
	all, err := db.ListFailures(false)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, u := range all {
		if u.Status == status {
			out = append(out, u)
		}
	}
	return out, nil
}

// BrowseFilter narrows a Browse list to the files one Statistics chart
// element counted: a capture-year bar, an extension row, a media-type
// segment, or an "uploaded over time" bar.
//
// The rule it exists to keep: a list opened from a bar contains EXACTLY the
// files that bar counted. A bar reading 931 that opens 912 files is worse
// than no link. So nothing here re-derives a chart's definition; each
// condition calls the function the chart itself uses -- CaptureYearLabel,
// extensions.KindOf/ExtOf, IsOutstanding, InActivityRange -- and a test
// holds every bucket's list to its count.
//
// Every field arrives from a URL. ParseBrowseFilter validates each one
// against a closed set and drops anything else, and the filtering happens
// in Go over rows already loaded, so no value is ever interpolated into
// SQL.
type BrowseFilter struct {
	// Kind is "photo", "video" or "other" (extensions.KindOf's three
	// outcomes; "other" is KindUnknown).
	Kind string
	// Ext is an extension exactly as extensions.ExtOf returns it:
	// lowercase, no dot, or "(none)".
	Ext string
	// Year is a capture-year bucket: "2019", "older" or "unknown".
	Year string
	// Outstanding keeps only work still ahead (IsOutstanding) -- the
	// media-type bars' pending segment.
	Outstanding bool
	// UploadedOn, with Span, selects one "uploaded over time" bar: the
	// bucket of that granularity containing this local day. Zero means no
	// upload-time condition.
	UploadedOn time.Time
	Span       ActivityGranularity
}

// Active reports whether the filter narrows anything.
func (f BrowseFilter) Active() bool {
	return f.Kind != "" || f.Ext != "" || f.Year != "" || f.Outstanding || !f.UploadedOn.IsZero()
}

var (
	browseFilterYear = regexp.MustCompile(`^[0-9]{4}$`)
	browseFilterExt  = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
)

// ParseBrowseFilter reads a filter from a query string, keeping only values
// it recognizes. An unknown value is dropped rather than rejected: the
// links that carry these are built by the Statistics page, so anything
// else is a hand-edited URL, and the banner shows exactly which conditions
// survived.
func ParseBrowseFilter(v url.Values) BrowseFilter {
	var f BrowseFilter
	switch k := v.Get("kind"); k {
	case "photo", "video", "other":
		f.Kind = k
	}
	if e := v.Get("ext"); browseFilterExt.MatchString(e) || e == "(none)" {
		f.Ext = e
	}
	if y := v.Get("year"); browseFilterYear.MatchString(y) || y == "older" || y == "unknown" {
		f.Year = y
	}
	f.Outstanding = v.Get("st") == "outstanding"
	if d, err := time.ParseInLocation("2006-01-02", v.Get("up"), time.Local); err == nil {
		f.UploadedOn = d
		f.Span = ParseActivityGranularity(v.Get("span"))
	}
	return f
}

// Values is the inverse of ParseBrowseFilter, for building links.
func (f BrowseFilter) Values() url.Values {
	v := url.Values{}
	if f.Kind != "" {
		v.Set("kind", f.Kind)
	}
	if f.Ext != "" {
		v.Set("ext", f.Ext)
	}
	if f.Year != "" {
		v.Set("year", f.Year)
	}
	if f.Outstanding {
		v.Set("st", "outstanding")
	}
	if !f.UploadedOn.IsZero() {
		v.Set("up", f.UploadedOn.Format("2006-01-02"))
		v.Set("span", string(f.Span))
	}
	return v
}

// OlderThan is the first year the capture-year chart shows on its own;
// its "Older" bar folds every real year before it. ok is false when the
// chart has no Older bar, in which case "older" matches nothing.
func OlderThan(db *statedb.DB) (year int, ok bool, err error) {
	s, err := BuildStatistics(db)
	if err != nil {
		return 0, false, err
	}
	if len(s.ByYear) < 2 || s.ByYear[0].Label != "Older" {
		return 0, false, nil
	}
	y, err := strconv.Atoi(s.ByYear[1].Label)
	if err != nil {
		return 0, false, nil
	}
	return y, true, nil
}

// matcher builds the predicate for one filter. "older" is the only
// condition that depends on the library as a whole -- which years the chart
// shows on their own -- so it is resolved once here, from the chart's own
// BuildStatistics, rather than per row.
func (f BrowseFilter) matcher(db *statedb.DB, now time.Time) (func(statedb.Upload) bool, error) {
	olderThan, hasOlder := 0, false
	if f.Year == "older" {
		var err error
		if olderThan, hasOlder, err = OlderThan(db); err != nil {
			return nil, err
		}
	}
	var from, to time.Time
	if !f.UploadedOn.IsZero() {
		from, to = ActivityRange(f.UploadedOn, f.Span)
	}
	kinds := map[string]extensions.Kind{"photo": extensions.KindPhoto, "video": extensions.KindVideo, "other": extensions.KindUnknown}

	return func(u statedb.Upload) bool {
		if f.Kind != "" && extensions.KindOf(u.FirstSourcePath) != kinds[f.Kind] {
			return false
		}
		if f.Ext != "" && extensions.ExtOf(u.FirstSourcePath) != f.Ext {
			return false
		}
		if f.Outstanding && !IsOutstanding(u.Status) {
			return false
		}
		if !f.UploadedOn.IsZero() && !InActivityRange(u, from, to) {
			return false
		}
		if f.Year != "" {
			label := CaptureYearLabel(u.FirstSourcePath, u.CapturedAt, now)
			switch f.Year {
			case "unknown":
				return label == "Unknown"
			case "older":
				y, err := strconv.Atoi(label)
				return hasOlder && err == nil && y < olderThan
			default:
				return label == f.Year
			}
		}
		return true
	}, nil
}
