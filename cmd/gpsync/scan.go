package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

// defaultScanConcurrency is deliberately conservative, not runtime.NumCPU().
// Hashing is disk-bound, and on a spinning HDD (common for a bulk photo
// archive) concurrent reads from multiple goroutines cause seek thrashing
// between each worker's file -- often *slower* than one file at a time,
// unlike CPU-bound work. This only helps when the source is SSD/NVMe; pass
// --concurrency to raise it if that's the case.
const defaultScanConcurrency = 2

func scanCmd() *cobra.Command {
	var concurrencyFlag int
	var mediaTypeFlag string
	cmd := &cobra.Command{
		Use:   "scan [folders...]",
		Short: "Find new and changed files and queue them for upload",
		Long: "Scans the given folders, or the configured source folders, and queues new and changed files. Unchanged files are not hashed again. Deleted and moved files are detected as well; see gpsync recheck.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync scan\n" +
			"  gpsync scan \"C:\\Photos\\2026\" --media-type videos",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			mediaKind, err := extensions.ParseKind(mediaTypeFlag)
			if err != nil {
				return err
			}
			scanner.SetMediaKindFilter(mediaKind)
			patterns := args
			if len(patterns) == 0 {
				patterns = cfg.SourceFolders
			}
			if len(patterns) == 0 {
				return fmt.Errorf("no folders given and none configured — run `gpsync setup` or pass folders explicitly")
			}
			// Scan concurrency is about local disk/CPU (hashing + EXIF reads), unrelated
			// to cfg.Concurrency which tunes network-bound upload parallelism -- default
			// to the machine's core count rather than reusing the upload setting.
			scanConcurrency := defaultScanConcurrency
			if concurrencyFlag > 0 {
				scanConcurrency = concurrencyFlag
			}

			// Expand the patterns into the actual leaf folders and walk them
			// one at a time, exactly as `gpsync sync` and `gpsync mark-synced`
			// already do. A single flat multi-pattern scan gave one combined
			// "X/Y files" counter with no folder name and no position in the
			// batch -- on a large library that's a progress bar that tells
			// you nothing about where you are.
			units, err := engine.DiscoverSyncUnits(patterns)
			if err != nil {
				return err
			}
			if len(units) == 0 {
				return fmt.Errorf("no folders containing files found under: %s", strings.Join(patterns, ", "))
			}
			fmt.Printf("Scanning %d folder(s)  (concurrency %d)\n", len(units), scanConcurrency)

			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			// Ctrl+C during a run must leave a record of WHY it stopped,
			// not a row stuck at status='running' forever.
			defer handleInterrupts(db)()
			reportAutoClean(db)

			overallStart := time.Now()
			var grandSeen, grandHashed, grandPending int
			for idx, folder := range units {
				printFolderHeader(idx+1, len(units), folder)
				start := time.Now()
				lastPrinted := time.Time{}
				summary, err := scanner.ScanFolders(db, []string{folder}, "manual_scan", scanConcurrency, false, true, printSkip, printPreScan, func(p scanner.ProgressEvent) {
					if time.Since(lastPrinted) < 200*time.Millisecond && p.Done != p.Total {
						return
					}
					lastPrinted = time.Now()
					elapsed := time.Since(start).Round(time.Second)
					rate := float64(p.Done) / time.Since(start).Seconds()
					fmt.Printf("\r  %s %d/%d  %s%s",
						colDim(fmt.Sprintf("[%s elapsed, ~%.0f/sec]", elapsed, rate)), p.Done, p.Total, colFile(formatInFlight(p.InFlight)), eol())
				})
				fmt.Println()
				if err != nil {
					return fmt.Errorf("%s: %w", folder, err)
				}
				fmt.Printf("  done in %s — %d files seen, %d hashed, %s newly queued\n",
					time.Since(start).Round(time.Second), summary.FilesSeen, summary.FilesHashed, colOK(fmt.Sprintf("%d", summary.NewPending)))
				grandSeen += summary.FilesSeen
				grandHashed += summary.FilesHashed
				grandPending += summary.NewPending
			}

			fmt.Printf("Done in %s. %d files seen across %d folders, %d hashed (new/changed), %s newly queued for upload.\n",
				time.Since(overallStart).Round(time.Second), grandSeen, len(units), grandHashed, colOK(fmt.Sprintf("%d", grandPending)))
			resolveMissingFiles(db, cfg, false)
			return nil
		},
	}
	cmd.Flags().IntVar(&concurrencyFlag, "concurrency", 0, "Files hashed in parallel (default 2; raise it for SSDs)")
	cmd.Flags().StringVar(&mediaTypeFlag, "media-type", "", "Only photos or videos (default: all)")
	return cmd
}

// ── upload ─────────────────────────────────────────────────────────────
