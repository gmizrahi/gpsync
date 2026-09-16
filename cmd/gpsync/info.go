package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gmizrahi/gpsync/internal/backup"
	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/quota"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

func infoCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "info",
		Short: "Show what is synced, pending and failed, and what to do next",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			// Resolves the unambiguous originals-folder review cases (see
			// engine.AutoResolveObviousOriginals) before reporting, so
			// "Needs review" below only ever counts genuinely ambiguous
			// items -- not ones that would auto-resolve the moment anyone
			// actually looked.
			if _, err := engine.AutoResolveObviousOriginals(db); err != nil {
				return err
			}

			s, err := db.LedgerSummary()
			if err != nil {
				return err
			}
			dq := quota.NewDailyQuota(db)
			used, _ := dq.Used()

			synced := s.UploadedByGPB + s.MarkedSynced
			fmt.Println(colHeader("Synced:"))
			fmt.Printf("  %s total  (%s uploaded by gpsync, %s marked-synced via `gpsync mark-synced`)\n",
				colOK(fmt.Sprintf("%d", synced)), fmt.Sprintf("%d", s.UploadedByGPB), fmt.Sprintf("%d", s.MarkedSynced))

			fmt.Println(colHeader("Pending:"))
			if s.Pending > 0 {
				fmt.Printf("  %s scanned but not yet uploaded — run `gpsync upload` (or `gpsync sync <folder>`) to push them\n",
					colWarn(fmt.Sprintf("%d", s.Pending)))
			} else {
				fmt.Println("  0")
			}

			fmt.Println(colHeader("Failed:"))
			if s.FailedPermanent > 0 {
				fmt.Printf("  %s permanent (won't retry — run `gpsync log --permanent` for why; usually an unsupported format)\n",
					colErr(fmt.Sprintf("%d", s.FailedPermanent)))
			}
			if s.FailedRetryable > 0 {
				fmt.Printf("  %s retryable — will be retried automatically on the next `gpsync upload`/`gpsync sync`\n",
					colWarn(fmt.Sprintf("%d", s.FailedRetryable)))
			}
			if s.FailedPermanent == 0 && s.FailedRetryable == 0 {
				fmt.Println("  0")
			}

			// Without this, a hash routed to needs_review (see
			// scanner.IsInOriginalsFolder) was counted in the Ledger
			// section's TotalHashes below but invisible everywhere else --
			// a real gap, since the whole point of the originals-folder
			// review feature is that these files need the user's
			// attention, not silence.
			fmt.Println(colHeader("Needs review:"))
			if s.NeedsReview > 0 {
				fmt.Printf("  %s file(s) (%s) found inside \"originals\" folders — open gpsync-tray's Originals tab to resolve, or run `gpsync clean-originals` for the CLI backlog\n",
					colWarn(fmt.Sprintf("%d", s.NeedsReview)), humanBytes(s.NeedsReviewBytes))
			} else {
				fmt.Println("  0")
			}

			fmt.Println(colHeader("Ledger:"))
			fmt.Printf("  %d distinct file hashes tracked (%s)\n", s.TotalHashes, humanBytes(s.TotalBytesTracked))
			fmt.Printf("  %d file paths seen across %d folders", s.TotalFilesSeen, s.TotalFolders)
			if dupes := s.TotalFilesSeen - s.TotalHashes; dupes > 0 {
				fmt.Printf("  (%d are duplicate content under a different name/location)", dupes)
			}
			fmt.Println()
			fmt.Printf("  Quota used today: %d/%d (resets midnight Pacific Time)\n", used, dq.DailyLimit())
			if uploadedCount, uploadedBytes, uerr := db.UploadedSince(quota.PacificMidnightEpoch()); uerr == nil {
				fmt.Printf("  Uploaded today: %d file(s), %s (resets midnight Pacific Time)\n", uploadedCount, humanBytes(uploadedBytes))
			}

			// Sync progress: how far along, and when it finishes at the
			// rate actually observed over the last week -- which already
			// includes time lost to throttle backoff, unlike any rate
			// derived from bandwidth. Best-effort: a forecast is a
			// convenience, never a reason to fail `gpsync info`.
			if fc, ferr := engine.BuildForecast(db, time.Now()); ferr == nil {
				switch {
				case fc.Done:
					fmt.Printf("  Sync progress: complete — %s uploaded\n", humanBytes(fc.SyncedBytes))
				case fc.Known:
					fmt.Printf("  Sync progress: %.1f%% (%s of %s), %s left across %d file(s)\n",
						fc.PercentBytes, humanBytes(fc.SyncedBytes), humanBytes(fc.SyncedBytes+fc.RemainingBytes),
						humanBytes(fc.RemainingBytes), fc.RemainingFiles)
					fmt.Printf("  Projected finish: %s (~%.0f days at %s/day over the last 7 days)\n",
						fc.ETA.Format("Mon 2 Jan"), fc.DaysRemaining, humanBytes(int64(fc.BytesPerDay)))
				default:
					fmt.Printf("  Sync progress: %.1f%% (%s of %s), %s left across %d file(s) — nothing uploaded in the last 7 days, so no finish estimate\n",
						fc.PercentBytes, humanBytes(fc.SyncedBytes), humanBytes(fc.SyncedBytes+fc.RemainingBytes),
						humanBytes(fc.RemainingBytes), fc.RemainingFiles)
				}
			}

			run, err := db.RunGet()
			if err == nil && run != nil && run.Status == "running" {
				fmt.Printf("  Active run (%s): %d/%d files, %d/%d folders\n",
					run.RunType, run.FilesDone, run.FilesTotal, run.FoldersDone, run.FoldersTotal)
			}
			if err == nil {
				printLastRunOutcome(run)
			}

			fmt.Println(colHeader("Backups:"))
			cfg, cfgErr := config.Load()
			if cfgErr != nil || cfg.BackupDir == "" {
				fmt.Println("  none configured — run `gpsync backup --dest <folder>`")
			} else if backups, lerr := backup.List(cfg.BackupDir); lerr != nil {
				fmt.Printf("  %s couldn't list %s: %v\n", colWarn("warning:"), cfg.BackupDir, lerr)
			} else if len(backups) == 0 {
				fmt.Printf("  none yet in %s — run `gpsync backup`\n", cfg.BackupDir)
			} else {
				latest := backups[len(backups)-1]
				if info, statErr := os.Stat(latest); statErr == nil {
					fmt.Printf("  Last backup: %s (%s ago)\n", info.ModTime().Format("2006-01-02 15:04:05"), time.Since(info.ModTime()).Round(time.Second))
				} else {
					fmt.Printf("  Last backup: %s\n", filepath.Base(latest))
				}
				fmt.Printf("  %d backup(s) kept in %s\n", len(backups), cfg.BackupDir)
			}
			return nil
		},
	}
}

func logCmd() *cobra.Command {
	var permanentFlag bool
	cmd := &cobra.Command{
		Use:   "log",
		Short: "Show failed uploads and their errors",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			failures, err := db.ListFailures(permanentFlag)
			if err != nil {
				return err
			}
			if len(failures) == 0 {
				fmt.Println("No failures.")
				return nil
			}
			for _, f := range failures {
				msg := ""
				if f.LastErrorMessage.Valid {
					msg = f.LastErrorMessage.String
				}
				fmt.Printf("%-60s %-18s %s\n", f.FirstSourcePath, f.Status, msg)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&permanentFlag, "permanent", false, "Only permanent failures")
	return cmd
}

func extensionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "extensions",
		Short: "Show the file extensions seen and how their uploads went",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			stats, err := db.ExtStats()
			if err != nil {
				return err
			}
			if len(stats) == 0 {
				fmt.Println("No extension data yet — run `gpsync upload` first.")
				return nil
			}
			fmt.Printf("%-12s %-10s %-10s %-10s %s\n", "Extension", "Attempted", "Succeeded", "Rejected", "Last error")
			for _, s := range stats {
				le := ""
				if s.LastError.Valid {
					le = s.LastError.String
				}
				fmt.Printf("%-12s %-10d %-10d %-10d %s\n", s.Ext, s.Attempted, s.Succeeded, s.Rejected, le)
			}
			return nil
		},
	}
}

// ── duplicates ─────────────────────────────────────────────────────────
