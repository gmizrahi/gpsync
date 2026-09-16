package engine

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

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
)

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
	default:
		return BrowseResult{}, fmt.Errorf("unknown file list kind %q", q.Kind)
	}
	if err != nil {
		return BrowseResult{}, err
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
