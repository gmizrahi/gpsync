package engine

import (
	"fmt"

	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// CleanOriginalsSummary reports what one pass found and did.
type CleanOriginalsSummary struct {
	Matched int
	Items   []statedb.NeedsReviewItem
}

// CleanOriginals backfills the originals-folder review state onto
// pending/failed_retryable rows a scan queued BEFORE that feature existed.
// They move to needs_review, where the Originals page offers
// queue-and-upload or add-to-ignore-list per file.
//
// db.OriginalsFolderRowsPendingOrFailed's SQL pre-filter is deliberately
// coarse -- a plain LIKE against the path, because statedb cannot import
// scanner without an import cycle -- so every candidate is re-checked here
// with the exact scanner.IsInOriginalsFolder test (immediate parent only,
// case-insensitive) before it counts.
//
// With commit false nothing is written and the summary reports what would
// change. Safe to run repeatedly.
func CleanOriginals(db *statedb.DB, commit bool) (CleanOriginalsSummary, error) {
	var sum CleanOriginalsSummary
	rows, err := db.OriginalsFolderRowsPendingOrFailed()
	if err != nil {
		return sum, err
	}
	for _, r := range rows {
		if !scanner.IsInOriginalsFolder(r.FirstSourcePath) {
			continue
		}
		sum.Matched++
		sum.Items = append(sum.Items, r)
		if !commit {
			continue
		}
		if err := db.MigrateToNeedsReview(r.SHA256); err != nil {
			return sum, fmt.Errorf("migrating %s to needs_review: %w", r.FirstSourcePath, err)
		}
	}
	return sum, nil
}
