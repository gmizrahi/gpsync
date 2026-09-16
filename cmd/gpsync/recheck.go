package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

type recheckOptions struct {
	revalidate  bool // decide flagged missing files now
	missing     bool // forget files confirmed missing
	unsupported bool // forget permanent failures with an unsupported format
	retry       bool // requeue permanent failures whose format is supported
}

// recheckSummary counts what one `gpsync recheck` pass found and did.
type recheckSummary struct {
	reappeared, relocated, confirmed, deferred int
	forgotten, forgottenUploaded               int
	forgottenBytes                             int64
	unsupported, unsupportedForgotten          int
	retryable, requeued                        int
	failedFileGone                             int
}

// recheckSampleSize caps how many paths the report lists per category.
const recheckSampleSize = 10

// recheck reviews missing files and permanent failures and applies whatever
// opts asks for. With no options it only reports.
//
// Missing files are decided by engine.ResolveMissingFiles first, and
// --missing re-checks each confirmed file on disk right before forgetting
// it. When a source folder is unavailable, --missing refuses outright:
// a file on an unplugged drive looks exactly like a deleted one.
func recheck(db *statedb.DB, sourceFolders []string, opts recheckOptions, now time.Time) (recheckSummary, error) {
	var sum recheckSummary

	if opts.revalidate || opts.missing {
		res, err := engine.ResolveMissingFiles(db, sourceFolders, now)
		if err != nil {
			return sum, err
		}
		if res.Skipped {
			if opts.missing {
				return sum, fmt.Errorf("not forgetting missing files: %s", res.SkipReason)
			}
			fmt.Printf("%s not checked: %s\n", colWarn("Missing files:"), res.SkipReason)
		}
		sum.reappeared, sum.relocated, sum.confirmed, sum.deferred = res.Reappeared, res.Relocated, res.Confirmed, res.Deferred
		if res.Changed() || res.Deferred > 0 {
			fmt.Printf("Re-checked missing files: %d back in place, %d moved (ledger updated), %d newly confirmed missing, %d undecided.\n",
				res.Reappeared, res.Relocated, res.Confirmed, res.Deferred)
		}
	}

	if opts.missing {
		rows, err := db.ConfirmedMissing()
		if err != nil {
			return sum, err
		}
		for _, r := range rows {
			if _, statErr := os.Stat(r.Path); statErr == nil {
				if err := db.ClearMissing(r.SHA256); err != nil {
					return sum, err
				}
				sum.reappeared++
				fmt.Printf("  %s %s\n", colOK("kept:"), r.Path+colDim(" (back on disk)"))
				continue
			} else if !errors.Is(statErr, fs.ErrNotExist) {
				fmt.Printf("  %s %s %s\n", colWarn("kept:"), r.Path, colDim("("+statErr.Error()+")"))
				continue
			}
			if err := db.DeleteUpload(r.SHA256); err != nil {
				return sum, fmt.Errorf("forgetting %s: %w", r.Path, err)
			}
			if err := db.DeleteFileSeen(r.Path); err != nil {
				return sum, fmt.Errorf("forgetting %s: %w", r.Path, err)
			}
			sum.forgotten++
			sum.forgottenBytes += r.Size
			if r.Status == "uploaded" {
				sum.forgottenUploaded++
			}
			fmt.Printf("  %s %s\n", colWarn("forgot:"), r.Path)
		}
	}

	failures, err := db.ListFailures(true)
	if err != nil {
		return sum, err
	}
	var unsupportedLeft, retryableLeft, goneLeft []string
	for _, f := range failures {
		path := f.FirstSourcePath
		if _, statErr := os.Stat(path); statErr != nil {
			sum.failedFileGone++
			goneLeft = append(goneLeft, path)
			continue
		}
		if extensions.Classify(path) == extensions.Unsupported {
			sum.unsupported++
			if !opts.unsupported {
				unsupportedLeft = append(unsupportedLeft, path)
				continue
			}
			if err := db.DeleteUpload(f.SHA256); err != nil {
				return sum, fmt.Errorf("forgetting %s: %w", path, err)
			}
			sum.unsupportedForgotten++
			fmt.Printf("  %s %s\n", colWarn("forgot:"), path+colDim(" (unsupported format; file not touched)"))
			continue
		}
		sum.retryable++
		if !opts.retry {
			retryableLeft = append(retryableLeft, path)
			continue
		}
		if _, err := db.ResetFailedToPending(f.SHA256); err != nil {
			return sum, fmt.Errorf("queueing %s: %w", path, err)
		}
		sum.requeued++
		fmt.Printf("  %s %s\n", colOK("queued:"), path)
	}

	if sum.forgotten > 0 {
		fmt.Printf("Forgot %d missing file(s), %s", sum.forgotten, humanBytes(sum.forgottenBytes))
		if sum.forgottenUploaded > 0 {
			fmt.Printf("; %d had been uploaded and remain in Google Photos", sum.forgottenUploaded)
		}
		fmt.Println(".")
	}
	if sum.unsupportedForgotten > 0 {
		fmt.Printf("Forgot %d unsupported-format failure(s).\n", sum.unsupportedForgotten)
	}
	if sum.requeued > 0 {
		fmt.Printf("Queued %d failure(s) for another attempt.\n", sum.requeued)
	}

	mc, err := db.MissingCounts()
	if err != nil {
		return sum, err
	}
	var confirmedLeft []string
	if mc.Confirmed > 0 {
		rows, err := db.ConfirmedMissing()
		if err != nil {
			return sum, err
		}
		for _, r := range rows {
			confirmedLeft = append(confirmedLeft, r.Path)
		}
	}

	if len(confirmedLeft)+mc.Flagged+len(unsupportedLeft)+len(retryableLeft)+len(goneLeft) == 0 {
		if sum.forgotten+sum.unsupportedForgotten+sum.requeued+sum.relocated+sum.reappeared == 0 {
			fmt.Println("Nothing to recheck.")
		}
		return sum, nil
	}
	fmt.Println()
	printRecheckGroup(fmt.Sprintf("%d file(s) confirmed missing from disk, %s", mc.Confirmed, humanBytes(mc.ConfirmedBytes)),
		"gpsync recheck --missing", confirmedLeft)
	if mc.Flagged > 0 {
		printRecheckGroup(fmt.Sprintf("%d file(s) flagged missing, not yet confirmed", mc.Flagged), "gpsync recheck --revalidate", nil)
	}
	printRecheckGroup(fmt.Sprintf("%d permanent failure(s) with an unsupported format", len(unsupportedLeft)), "gpsync recheck --unsupported", unsupportedLeft)
	printRecheckGroup(fmt.Sprintf("%d permanent failure(s) that can be retried", len(retryableLeft)), "gpsync recheck --retry", retryableLeft)
	printRecheckGroup(fmt.Sprintf("%d permanent failure(s) whose file is gone", len(goneLeft)), "the next scan flags them as missing", goneLeft)
	return sum, nil
}

func printRecheckGroup(title, action string, paths []string) {
	if strings.HasPrefix(title, "0 ") {
		return
	}
	fmt.Printf("%s  %s\n", colHeader(title), colDim("-> "+action))
	for i, p := range paths {
		if i == recheckSampleSize {
			fmt.Printf("  %s\n", colDim(fmt.Sprintf("... and %d more", len(paths)-recheckSampleSize)))
			break
		}
		fmt.Printf("  %s\n", p)
	}
}

func recheckCmd() *cobra.Command {
	var opts recheckOptions
	var all bool
	cmd := &cobra.Command{
		Use:   "recheck",
		Short: "Review missing files and permanent failures, and clean them up",
		Long: "Without flags, reports what needs attention and changes nothing.\n\n" +
			"Missing files are ledger entries whose file is no longer on disk. Scans flag them; " +
			"they are confirmed once no moved copy exists under the source folders. " +
			"Forgetting an entry never touches Google Photos.\n\n" +
			"Examples:\n" +
			"  gpsync recheck                 Report only\n" +
			"  gpsync recheck --revalidate    Re-check flagged files, e.g. after reattaching a drive\n" +
			"  gpsync recheck --missing       Forget files confirmed missing\n" +
			"  gpsync recheck --retry         Queue failures that can be retried\n" +
			"  gpsync recheck --all           Same as --missing --unsupported --retry",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all {
				opts.missing, opts.unsupported, opts.retry = true, true, true
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			_, err = recheck(db, cfg.SourceFolders, opts, time.Now())
			return err
		},
	}
	cmd.Flags().BoolVar(&opts.revalidate, "revalidate", false, "Re-check flagged files; clear those back on disk or moved")
	cmd.Flags().BoolVar(&opts.missing, "missing", false, "Forget files confirmed missing (each is re-checked first)")
	cmd.Flags().BoolVar(&opts.unsupported, "unsupported", false, "Forget permanent failures with an unsupported format (files are not touched)")
	cmd.Flags().BoolVar(&opts.retry, "retry", false, "Queue permanent failures whose format is supported for another attempt")
	cmd.Flags().BoolVar(&all, "all", false, "Same as --missing --unsupported --retry")
	return cmd
}
