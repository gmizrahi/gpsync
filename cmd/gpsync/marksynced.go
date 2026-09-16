package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

func markSyncedCmd() *cobra.Command {
	var concurrencyFlag int
	var quality string
	var unmark bool
	cmd := &cobra.Command{
		Use:   "mark-synced <folder|file|glob> [more...]",
		Short: "Mark files as already in Google Photos, without uploading",
		Long: "Hashes the given files and records them as uploaded, without contacting Google. Use it for files uploaded some other way, such as the Google Photos app or rclone.\n" +
			"\n" +
			"Accepts folders (including subfolders), files and globs; quote globs in PowerShell. A folder is marked as it is now: files added later are scanned as new. Unsupported and system files are skipped.\n" +
			"\n" +
			"--unmark queues the files for upload again, whether they were marked or actually uploaded.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync mark-synced \"C:\\Photos\\2019\"\n" +
			"  gpsync mark-synced \"C:\\Photos\\2022\\Holiday\\*.mp4\" --quality original\n" +
			"  gpsync mark-synced \"C:\\Photos\\2019\" --unmark",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			units, namedFiles, err := engine.DiscoverMarkTargets(args)
			if err != nil {
				return err
			}
			if len(units) == 0 && len(namedFiles) == 0 {
				return fmt.Errorf("no files or folders found matching: %s", strings.Join(args, ", "))
			}

			cfg, err := config.Load()
			if err != nil {
				return err
			}
			// --quality falls back to the configured upload_quality. The
			// two are different facts (config says what
			// gpsync WILL send; this records what is already stored at
			// Google), so the fallback is announced in the output rather
			// than applied silently -- if they ever disagree, the person
			// running the command is the only one who can tell.
			qualityArg, qualitySource := quality, "--quality"
			if qualityArg == "" {
				qualityArg, qualitySource = cfg.UploadQuality, "config upload_quality"
			}
			recordQuality := statedb.QualityFor(qualityArg)
			if quality != "" && recordQuality == "" {
				return fmt.Errorf("unknown quality %q: use \"original\" or \"storage-saver\"", quality)
			}

			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			// Ctrl+C during a run must leave a record of WHY it stopped,
			// not a row stuck at status='running' forever.
			defer handleInterrupts(db)()

			if unmark {
				// No scanning, no hashing: this only reverses a previous
				// decision, and the ledger already holds what that needs.
				var res statedb.UnmarkResult
				if len(units) > 0 {
					got, uerr := db.UnmarkSyncedUnder(units)
					if uerr != nil {
						return uerr
					}
					res.Asserted += got.Asserted
					res.Uploaded += got.Uploaded
					res.UploadedBytes += got.UploadedBytes
				}
				if len(namedFiles) > 0 {
					got, uerr := db.UnmarkSyncedPaths(namedFiles)
					if uerr != nil {
						return uerr
					}
					res.Asserted += got.Asserted
					res.Uploaded += got.Uploaded
					res.UploadedBytes += got.UploadedBytes
				}
				total := res.Asserted + res.Uploaded
				fmt.Printf("Unmarked %d file(s) — back to pending, so the next upload run sends them.\n", total)
				if total == 0 {
					fmt.Println(colDim("  (nothing matched — these may already be pending, or never marked at all)"))
					return nil
				}
				fmt.Printf("  %s\n", colDim(fmt.Sprintf("%d were mark-synced assertions, %d were real gpsync uploads",
					res.Asserted, res.Uploaded)))
				// Not a refusal, just the number that decides whether this
				// was the intended command: re-sending is how a
				// storage-saver copy gets upgraded to an original, but it
				// is also how a whole library gets re-sent by accident.
				if res.Uploaded > 0 {
					fmt.Printf("  %s\n", colWarn(fmt.Sprintf("that re-sends %s already stored at Google (they merge in place, not duplicate)",
						humanBytes(res.UploadedBytes))))
				}
				return nil
			}

			scanConcurrency := defaultScanConcurrency
			if concurrencyFlag > 0 {
				scanConcurrency = concurrencyFlag
			}

			// Directly-named files are scanned as ONE extra group rather
			// than one-at-a-time: a glob like "*.mp4" can name dozens, and
			// a separator-plus-header each would bury the actual result.
			groups := make([][]string, 0, len(units)+1)
			labels := make([]string, 0, len(units)+1)
			for _, folder := range units {
				groups = append(groups, []string{folder})
				labels = append(labels, folder)
			}
			if len(namedFiles) > 0 {
				groups = append(groups, namedFiles)
				labels = append(labels, fmt.Sprintf("%d file(s) named directly", len(namedFiles)))
			}

			var grandMarked, grandSeen int
			for idx, group := range groups {
				fmt.Println(colDim(strings.Repeat("─", separatorWidth)))
				fmt.Println(colHeader(fmt.Sprintf("[%d/%d] %s", idx+1, len(groups), labels[idx])))
				start := time.Now()
				lastPrinted := time.Time{}
				summary, err := scanner.ScanFolders(db, group, "mark_synced", scanConcurrency, true, true, printSkip, printPreScan, func(p scanner.ProgressEvent) {
					if time.Since(lastPrinted) < 200*time.Millisecond && p.Done != p.Total {
						return
					}
					lastPrinted = time.Now()
					fmt.Printf("\r  %s %d/%d  %s%s",
						colDim(fmt.Sprintf("[%s elapsed]", time.Since(start).Round(time.Second))), p.Done, p.Total, colFile(formatInFlight(p.InFlight)), eol())
				})
				fmt.Println()
				if err != nil {
					return fmt.Errorf("%s: %w", labels[idx], err)
				}
				fmt.Printf("  done in %s — %d files marked as already synced (%d already known)\n",
					time.Since(start).Round(time.Second), summary.NewPending, summary.FilesSeen-summary.NewPending)
				grandMarked += summary.NewPending
				grandSeen += summary.FilesSeen
			}
			fmt.Printf("Done. %d of %d files across %d group(s) newly marked as synced.\n", grandMarked, grandSeen, len(groups))
			if recordQuality == "" {
				fmt.Printf("  %s\n", colWarn(fmt.Sprintf("quality NOT recorded: %s is %q, which is neither original nor storage-saver",
					qualitySource, qualityArg)))
			} else {
				q := recordQuality
				// Folders match by prefix; directly-named files match by
				// EXACT path. Running a file glob through the prefix form
				// would build "...\\*.mp4\\%" and match nothing, silently
				// recording no quality at all for precisely the argument
				// shape this file support exists to serve.
				var n int64
				if len(units) > 0 {
					got, qerr := db.SetRemoteQuality(q, units)
					if qerr != nil {
						return qerr
					}
					n += got
				}
				if len(namedFiles) > 0 {
					got, qerr := db.SetRemoteQualityForPaths(q, namedFiles)
					if qerr != nil {
						return qerr
					}
					n += got
				}
				label := "original"
				if q == statedb.QualityStorageSaver {
					label = "storage-saver"
				}
				fmt.Printf("Recorded %d file(s) as %s quality in Google Photos %s\n",
					n, colOK(label), colDim("(from "+qualitySource+")"))
				counts, cerr := db.RemoteQualityCounts()
				if cerr == nil {
					fmt.Printf("  %s\n", colDim(fmt.Sprintf("ledger now: %d original, %d storage-saver, %d unrecorded",
						counts[statedb.QualityOriginal], counts[statedb.QualityStorageSaver], counts[""])))
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&concurrencyFlag, "concurrency", 0, "Files hashed in parallel (default 2; raise it for SSDs)")
	// The ledger cannot work quality out for itself -- the Photos API has
	// no storage-tier field -- so the only source is the person who put
	// the files there. Recording it is what lets `gpsync reupload` target
	// the right files instead of guessing, which it got wrong once by
	// treating every mark-synced row as suspect.
	cmd.Flags().StringVar(&quality, "quality", "", "Quality of the existing copies: original or storage-saver (default: upload_quality in config)")
	cmd.Flags().BoolVar(&unmark, "unmark", false, "Queue the files for upload again")
	return cmd
}

// ── whatis ─────────────────────────────────────────────────────────────
