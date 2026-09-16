package engine

import (
	"path/filepath"

	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// OriginalsCandidate is one file elsewhere in the library sharing a review
// item's filename -- shown to the user for visual confirmation before they
// decide whether to queue or ignore the originals-folder copy: the same
// filename is searched for across the whole library and shown alongside.
type OriginalsCandidate struct {
	Path   string
	Status string
}

// OriginalsReviewItem is one file awaiting the originals-folder review
// decision, plus the candidate matches found for it.
type OriginalsReviewItem struct {
	SHA256          string
	Size            int64
	FirstSourcePath string
	Candidates      []OriginalsCandidate
}

// BuildOriginalsReviewItem assembles one review item by hash, including
// its basename candidates from elsewhere in the library. ok is false if
// this hash is no longer a needs_review row -- e.g. a stale page reload
// after it was already resolved (by this request or a concurrent one) --
// distinct from statedb.ResolveNeedsReview's own SQL-level guard, which
// protects the RESOLVE action; this is the equivalent check for rendering.
func BuildOriginalsReviewItem(db *statedb.DB, sha256 string) (OriginalsReviewItem, bool, error) {
	items, err := db.NeedsReviewItems()
	if err != nil {
		return OriginalsReviewItem{}, false, err
	}
	var found *statedb.NeedsReviewItem
	for i := range items {
		if items[i].SHA256 == sha256 {
			found = &items[i]
			break
		}
	}
	if found == nil {
		return OriginalsReviewItem{}, false, nil
	}

	matches, err := db.FindByBasename(filepath.Base(found.FirstSourcePath), sha256)
	if err != nil {
		return OriginalsReviewItem{}, false, err
	}
	item := OriginalsReviewItem{
		SHA256:          found.SHA256,
		Size:            found.Size,
		FirstSourcePath: found.FirstSourcePath,
	}
	for _, m := range matches {
		item.Candidates = append(item.Candidates, OriginalsCandidate{Path: m.Path, Status: m.Status})
	}
	return item, true, nil
}

// AutoResolveObviousOriginals sweeps every needs_review item and
// auto-resolves the ONE structurally certain case: a file inside an
// originals folder whose match sits at the immediate parent level, the
// same convention Picasa follows with its .picasaoriginals subfolder.
//
//   - An immediate-parent sibling (scanner.SiblingPath) is tracked
//     ANYWHERE in files_seen -> ignored. This IS the .picasaoriginals
//     pattern, just in a visible folder instead of a hidden one -- no
//     review needed, the match is structurally certain.
//   - Anything else -- no match anywhere in the library, OR a same-named
//     file somewhere else entirely -- is left in needs_review for a
//     human. An EARLIER version of this function also auto-queued the
//     "no match anywhere" case, on the assumption that nothing suggests
//     it's a duplicate of anything. That was wrong: a file with no match
//     can just as easily be something deliberately discarded into the
//     originals
//     folder (a blurry shot, a bad take) as a genuinely new photo that
//     happens to sit in an oddly-named folder, and only a human can tell
//     which. The review UI already handles "no candidates found"
//     gracefully (shows that plainly, still offers both Queue-and-Upload
//     and Add-to-Ignore-List), so no separate handling was needed here --
//     just NOT auto-resolving it.
//
// Safe and cheap to call repeatedly -- an item whose sibling hasn't been
// scanned yet (a real possibility: originals-folder files can be
// discovered before or after their sibling within the same walk, and
// siblings scanned in a LATER, separate scan wouldn't be visible yet
// either) simply stays in needs_review until the next call after that
// sibling exists. Called before every place that shows or counts
// needs_review items (the dashboard's Originals tab, `gpsync info`, `gpsync
// clean-originals`) rather than hooked into scanning itself, so
// correctness depends on when the list is actually LOOKED AT, not on
// guessing the right moment during a scan.
//
// Returns byte totals alongside the count, so a caller can report both
// without a second pass over the same data.
func AutoResolveObviousOriginals(db *statedb.DB) (AutoResolveSummary, error) {
	var sum AutoResolveSummary
	items, err := db.NeedsReviewItems()
	if err != nil {
		return sum, err
	}
	for _, it := range items {
		sibling := scanner.SiblingPath(it.FirstSourcePath)
		seen, err := db.GetFileSeen(sibling)
		if err != nil {
			return sum, err
		}
		if seen == nil {
			continue
		}
		if err := db.ResolveNeedsReview(it.SHA256, false); err != nil {
			return sum, err
		}
		sum.IgnoredCount++
		sum.IgnoredBytes += it.Size
	}
	return sum, nil
}

// AutoResolveSummary reports what AutoResolveObviousOriginals did.
type AutoResolveSummary struct {
	IgnoredCount int
	IgnoredBytes int64
}
