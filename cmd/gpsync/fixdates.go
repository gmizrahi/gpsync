package main

import (
	"fmt"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

// fixDatesSummary reports what gpsync fix-dates found/changed.
type fixDatesSummary struct {
	fixed     int // path-derived date disagreed with the stored one, and got overwritten
	unchanged int // no plausible date in the path, or it already agreed with what's stored
}

// fixDates re-derives captured_at for every tracked file from its own
// path (scanner.DateFromPath) and overwrites the stored value whenever
// the two disagree on YEAR. It fixes per-year counts that came out wrong
// because the stored date had drifted from the file's own path. Root
// cause: scanner.captureDate falls back to the file's filesystem
// MTIME whenever EXIF is missing/unreadable, and mtime can drift
// arbitrarily far from the true capture date (a later backup/migration
// touching an EXIF-less file long after it was actually taken). This
// backfill is what fixes files that were already scanned BEFORE that
// fallback ordering changed -- captureDate() itself now tries a
// path-derived date before mtime, but that only helps FUTURE scans; a
// file already in the ledger keeps whatever captured_at it got on its
// FIRST scan (EnsurePending is `ON CONFLICT DO NOTHING`) until something
// like this explicitly revisits it.
//
// Deliberately compares by YEAR, not exact timestamp: a file whose
// stored captured_at already agrees on the year (whether from genuine
// EXIF or a mtime that happens to still be close enough) is left alone,
// so this doesn't churn every row on every run -- only ones the bug
// actually reached. gpsync-tray's Statistics page already applies this
// same path-first heuristic live for its own year chart regardless of
// whether this has been run, but nothing else that reads captured_at
// (Browse's Captured column, `gpsync whatis`) benefits
// until the ledger itself is fixed here.
//
// Split out from the cobra command so the decision logic is testable
// without a terminal, matching recheckFailures' own precedent.
func fixDates(db *statedb.DB, dryRun bool) (fixDatesSummary, error) {
	sum, err := engine.FixDates(db, dryRun)
	return fixDatesSummary{fixed: sum.Fixed, unchanged: sum.Unchanged}, err
}

func fixDatesCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "fix-dates",
		Short: "Correct capture dates from the dates in folder and file names",
		Long: "Files without readable EXIF get their capture date from the file's modified time, which copying and backups can change. This reads the date from the path instead (a 2002 or 2002_07 folder, or a name such as IMG-20220701-WA0060.jpg) and updates the stored date wherever the year differs.\n" +
			"\n" +
			"Safe to run repeatedly.",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			if dryRun {
				fmt.Println(colWarn("--dry-run: showing what would change, writing nothing."))
			}
			sum, err := fixDates(db, dryRun)
			if err != nil {
				return err
			}
			verb := "Fixed"
			if dryRun {
				verb = "Would fix"
			}
			fmt.Printf("%s the capture date for %s file(s); %s left unchanged (already correct, or nothing plausible in the path).\n",
				verb, colOK(fmt.Sprintf("%d", sum.fixed)), colDim(fmt.Sprintf("%d", sum.unchanged)))
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would change without writing")
	return cmd
}
