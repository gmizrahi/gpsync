package main

import (
	"fmt"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

// cleanOriginalsSummary reports what `gpsync clean-originals` found/did.
type cleanOriginalsSummary struct {
	matched int
	items   []statedb.NeedsReviewItem
}

// cleanOriginals backfills the originals-folder review state (see
// scanner.IsInOriginalsFolder) onto pending/failed_retryable rows a scan
// queued BEFORE that feature existed. They move to the needs_review
// state, where the tray's Originals tab offers queue-and-upload or
// add-to-ignore-list per file, the same as the Duplicate Resolver.
//
// db.OriginalsFolderRowsPendingOrFailed's own SQL pre-filter is coarse (a
// plain LIKE match against the path -- see its own doc comment for why:
// this package can't import scanner without a cycle); every candidate is
// re-checked here with the exact scanner.IsInOriginalsFolder test
// (immediate parent only, case-insensitive) before it counts as a real
// match. Split out from the cobra command so the decision logic is
// testable without a terminal, matching fixDates' own precedent.
func cleanOriginals(db *statedb.DB, commit bool) (cleanOriginalsSummary, error) {
	var sum cleanOriginalsSummary
	rows, err := db.OriginalsFolderRowsPendingOrFailed()
	if err != nil {
		return sum, err
	}
	for _, r := range rows {
		if !scanner.IsInOriginalsFolder(r.FirstSourcePath) {
			continue
		}
		sum.matched++
		sum.items = append(sum.items, r)
		if !commit {
			continue
		}
		if err := db.MigrateToNeedsReview(r.SHA256); err != nil {
			return sum, fmt.Errorf("migrating %s to needs_review: %w", r.FirstSourcePath, err)
		}
	}
	return sum, nil
}

func cleanOriginalsCmd() *cobra.Command {
	var commit bool
	cmd := &cobra.Command{
		Use:   "clean-originals",
		Short: "Move queued files from \"originals\" folders into review",
		Long: "Finds pending and retryable files inside an \"originals\" folder (the pre-edit copies some photo editors keep) and moves them into review. Review them in gpsync-tray's Originals tab, where each one can be queued or ignored.\n" +
			"\n" +
			"Shows what would change unless --commit is given.",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			if !commit {
				fmt.Println(colWarn("dry-run (default): showing what would change, writing nothing. Pass --commit to apply."))
			}
			sum, err := cleanOriginals(db, commit)
			if err != nil {
				return err
			}
			if sum.matched == 0 {
				fmt.Println(`Nothing to do -- no pending/failed_retryable files found inside an "originals" folder.`)
				return nil
			}
			var matchedBytes int64
			for _, it := range sum.items {
				matchedBytes += it.Size
			}
			verb := "Moved"
			if !commit {
				verb = "Would move"
			}
			fmt.Printf("%s %s file(s) (%s) from the upload queue into review:\n",
				verb, colOK(fmt.Sprintf("%d", sum.matched)), humanBytes(matchedBytes))
			for _, it := range sum.items {
				fmt.Printf("  %s\n", it.FirstSourcePath)
			}
			fmt.Println(colHeader("Grand total:"), fmt.Sprintf("%d file(s), %s", sum.matched, humanBytes(matchedBytes)))
			if !commit {
				fmt.Println(colDim("Re-run with --commit to apply."))
				return nil
			}

			// Resolves the unambiguous cases across the WHOLE needs_review
			// queue (see engine.AutoResolveObviousOriginals) right away --
			// not scoped to just the rows this run migrated, since the
			// sweep itself isn't either -- rather than leaving them
			// sitting there until someone next opens the dashboard.
			resolved, err := engine.AutoResolveObviousOriginals(db)
			if err != nil {
				return err
			}
			stillPending, err := db.NeedsReviewItems()
			if err != nil {
				return err
			}
			var stillPendingBytes int64
			for _, it := range stillPending {
				stillPendingBytes += it.Size
			}

			fmt.Println()
			fmt.Println(colHeader("Review queue after auto-resolving the certain case:"))
			fmt.Printf("  Ignored (edited copy found next to originals folder):  %s\n",
				colOK(fmt.Sprintf("%d file(s), %s", resolved.IgnoredCount, humanBytes(resolved.IgnoredBytes))))
			fmt.Printf("  Still needs review (no match, or match found elsewhere): %s\n",
				colWarn(fmt.Sprintf("%d file(s), %s", len(stillPending), humanBytes(stillPendingBytes))))
			if len(stillPending) > 0 {
				fmt.Println(colDim("Open gpsync-tray's Originals tab to resolve what's left -- including files with no match anywhere, which may be intentional discards, not new photos."))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&commit, "commit", false, "Apply the changes")
	return cmd
}

func originalsUploadedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "originals-uploaded",
		Short: "List uploaded files that came from an \"originals\" folder",
		Long: "Lists files already uploaded from inside an \"originals\" folder. These are likely duplicates of their edited versions in Google Photos. Each line says whether an edited copy is tracked next to the folder; if it is, the file is almost certainly a duplicate.\n" +
			"\n" +
			"gpsync cannot delete from Google Photos; remove duplicates there by hand. Changes nothing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			candidates, err := db.OriginalsFolderRowsUploaded()
			if err != nil {
				return err
			}
			var rows []statedb.UploadedOriginalsRow
			for _, r := range candidates {
				if scanner.IsInOriginalsFolder(r.FirstSourcePath) {
					rows = append(rows, r)
				}
			}
			if len(rows) == 0 {
				fmt.Println(`Nothing found -- no uploaded files are currently tracked inside an "originals" folder.`)
				return nil
			}

			var totalBytes int64
			var likelyCount, unconfirmedCount int
			var likelyBytes, unconfirmedBytes int64

			fmt.Printf("%s file(s) already uploaded from an \"originals\" folder:\n", colWarn(fmt.Sprintf("%d", len(rows))))
			for _, r := range rows {
				totalBytes += r.Size
				sibling := scanner.SiblingPath(r.FirstSourcePath)
				seen, serr := db.GetFileSeen(sibling)
				confidence := colDim("no edited copy found next to it -- verify before deleting")
				if serr == nil && seen != nil {
					confidence = colOK("edited copy also tracked -- likely a real duplicate")
					likelyCount++
					likelyBytes += r.Size
				} else {
					unconfirmedCount++
					unconfirmedBytes += r.Size
				}
				fmt.Printf("  %s  (%s, %s)\n", r.FirstSourcePath, humanBytes(r.Size), confidence)
				if r.MediaItemID != "" {
					fmt.Printf("    media item id: %s\n", r.MediaItemID)
				} else {
					fmt.Printf("    %s\n", colDim("no media item id -- uploaded via `gpsync mark-synced`"))
				}
			}

			fmt.Println()
			fmt.Println(colHeader("Grand total:"), fmt.Sprintf("%d file(s), %s", len(rows), humanBytes(totalBytes)))
			fmt.Printf("  %s likely real duplicates (edited copy also tracked): %d file(s), %s\n",
				colOK("→"), likelyCount, humanBytes(likelyBytes))
			fmt.Printf("  %s unconfirmed (verify before deleting): %d file(s), %s\n",
				colDim("→"), unconfirmedCount, humanBytes(unconfirmedBytes))
			return nil
		},
	}
}

// ── status / log / extensions ─────────────────────────────────────────
