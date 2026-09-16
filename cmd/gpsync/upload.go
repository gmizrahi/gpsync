package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/quota"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/uploader"
	"github.com/spf13/cobra"
)

// uploadFolders drives `gpsync upload`'s folder-by-folder loop, the mirror of
// syncFolders. Both callbacks are injected for the same reason: so the
// loop's stop-the-whole-invocation behavior can be tested without a
// network or OAuth credentials.
//
// hasWork is consulted before the folder header is printed, so a folder
// with nothing left to do produces no output at all rather than a header
// over an empty block.
//
// As in syncFolders, uploader.ErrRunAborted ends the loop: the daily quota
// and Google's throttle are both project-wide, so the folders after this
// one have nothing different waiting for them.
func uploadFolders(folders []engine.PendingFolder, hasWork func(folder string) (bool, error), uploadOne func(folder string) (uploader.Stats, error)) (uploader.Stats, error) {
	var stats uploader.Stats
	for idx, f := range folders {
		work, err := hasWork(f.Path)
		if err != nil {
			return stats, err
		}
		if !work {
			continue
		}
		printFolderHeader(idx+1, len(folders), f.Path)
		folderStats, err := uploadOne(f.Path)
		// Fold the counts in either way -- an aborted run still did real
		// work before it stopped.
		stats.Uploaded += folderStats.Uploaded
		stats.FailedPermanent += folderStats.FailedPermanent
		stats.FailedRetryable += folderStats.FailedRetryable
		stats.SkippedQuota += folderStats.SkippedQuota
		stats.SkippedMediaType += folderStats.SkippedMediaType
		if err != nil {
			if errors.Is(err, uploader.ErrRunAborted) {
				printRunAborted(err, idx+1, len(folders))
				return stats, nil
			}
			return stats, err
		}
	}
	return stats, nil
}

func uploadCmd() *cobra.Command {
	var concurrencyFlag int
	var qualityFlag string
	var smallestFirstFlag bool
	var smallestFirstGlobalFlag bool
	var mediaTypeFlag string
	cmd := &cobra.Command{
		Use:   "upload [folders...]",
		Short: "Upload pending files",
		Long: "Uploads all pending files, or only those under the given folders. Stays within the daily quota and retries after throttling.\n" +
			"\n" +
			"--smallest-first uploads smaller files first within each folder; --smallest-first-global does so across all folders.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync upload\n" +
			"  gpsync upload \"C:\\Photos\\2026\" --media-type photos\n" +
			"  gpsync upload --smallest-first-global",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if concurrencyFlag > 0 {
				cfg.Concurrency = concurrencyFlag
			}
			if qualityFlag != "" {
				cfg.UploadQuality = qualityFlag
			}
			if smallestFirstFlag || smallestFirstGlobalFlag {
				cfg.SortSmallestFirst = true
			}
			mediaKind, err := extensions.ParseKind(mediaTypeFlag)
			if err != nil {
				return err
			}
			cfg.MediaTypeFilter = mediaKind

			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			// Ctrl+C during a run must leave a record of WHY it stopped,
			// not a row stuck at status='running' forever.
			defer handleInterrupts(db)()
			reportAutoClean(db)

			dq := quota.NewDailyQuota(db)
			used, _ := dq.Used()
			fmt.Printf("Quota used today: %d/%d\n", used, dq.DailyLimit())
			if len(args) > 0 {
				fmt.Printf("Scoped to: %s\n", strings.Join(args, ", "))
			}

			// Break the pending backlog down by folder and upload one folder
			// at a time, so the live view shows "Totals for batch" plus a
			// real "For folder: <name>" the way `gpsync sync` does. A single
			// flat run over everything reported one anonymous
			// "(entire library)" block -- "5/4720 files" with no indication
			// of where in a 27GB library those five files were.
			folders, err := engine.PendingFolders(db, args)
			if err != nil {
				return err
			}
			if len(folders) == 0 {
				fmt.Println("Nothing pending to upload.")
				return nil
			}

			var totalFiles int
			var totalBytes int64
			for _, f := range folders {
				totalFiles += f.Files
				totalBytes += f.Bytes
			}
			// alreadySynced is 0/0 here, unlike `gpsync sync`: there's no
			// pre-scan estimate to reconcile against, ListPendingUnder
			// already reports exactly what's outstanding.
			batch := newBatchTracker(totalFiles, totalBytes, 0, 0)
			// Shared across every folder in this invocation, same as batch
			// -- see runUploadWithDashboard.
			conc := quota.NewAdaptiveConcurrency(cfg.Concurrency)
			breaker := uploader.NewCircuitBreakerState()

			start := time.Now()
			var stats uploader.Stats
			if smallestFirstGlobalFlag {
				// One flat call across every matched folder at once --
				// deliberately not the uploadFolders loop below, which
				// finishes one folder's queue before starting the next.
				// ListPendingUnder(args) already returns every pending row
				// under all of them in one slice, and Run() sorts THAT
				// whole slice by size when cfg.SortSmallestFirst is set --
				// so nothing folder-scoped survives to constrain the order.
				label := "(entire library, smallest-first)"
				if len(args) > 0 {
					label = strings.Join(args, ", ") + " (smallest-first, all at once)"
				}
				stats, err = runUploadWithDashboard(db, cfg, args, batch, conc, breaker, label)
				if err != nil {
					if !errors.Is(err, uploader.ErrRunAborted) {
						return err
					}
					printRunAborted(err, 1, 1)
				}
			} else {
				stats, err = uploadFolders(folders,
					func(folder string) (bool, error) {
						// A folder's scope covers its subfolders too, so if this
						// folder's pending files were already swept up by an
						// earlier (ancestor) entry in this same loop, there's
						// nothing left to do.
						remaining, lerr := db.ListPendingUnder([]string{folder})
						return len(remaining) > 0, lerr
					},
					func(folder string) (uploader.Stats, error) {
						return runUploadWithDashboard(db, cfg, []string{folder}, batch, conc, breaker, "")
					})
				if err != nil {
					return err
				}
			}

			fmt.Printf("Done in %s. Uploaded: %d  Failed (permanent): %d  Failed (will retry next run): %d  Skipped (quota): %d  Skipped (media-type): %d\n",
				time.Since(start).Round(time.Second), stats.Uploaded, stats.FailedPermanent, stats.FailedRetryable, stats.SkippedQuota, stats.SkippedMediaType)
			if stats.SkippedQuota > 0 {
				hrs := quota.SecondsUntilPacificMidnight() / 3600
				fmt.Printf("Daily quota reached — resets in ~%.1fh (midnight Pacific Time). Run `gpsync upload` again after reset.\n", hrs)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&concurrencyFlag, "concurrency", 0, "Parallel uploads (overrides config)")
	cmd.Flags().StringVar(&qualityFlag, "quality", "", "Upload quality this run: original or space_saver (default: config)")
	cmd.Flags().BoolVar(&smallestFirstFlag, "smallest-first", false, "Smallest files first, within each folder")
	cmd.Flags().BoolVar(&smallestFirstGlobalFlag, "smallest-first-global", false, "Smallest files first, across all folders")
	cmd.Flags().StringVar(&mediaTypeFlag, "media-type", "", "Only photos or videos this run (default: all)")
	return cmd
}

// ── mark-synced ────────────────────────────────────────────────────────
