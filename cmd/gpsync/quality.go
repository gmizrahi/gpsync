package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

// parseRemoteQuality maps the user-facing flag spelling to the stored
// value. Only these two are accepted: "unknown" is the absence of a
// record, not a third state to assert.
func parseRemoteQuality(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "original":
		return statedb.QualityOriginal, nil
	case "storage-saver", "storage_saver", "saver":
		return statedb.QualityStorageSaver, nil
	default:
		return "", fmt.Errorf("unknown quality %q: use \"original\" or \"storage-saver\"", s)
	}
}

// qualityCmd records what the remote copies of ALREADY-synced files are,
// for libraries marked before --quality existed. gpsync cannot work this
// out on its own: the Photos API exposes no storage-tier field, so the
// only source is the person who uploaded them.
func qualityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "quality <original|storage-saver> [folders...]",
		Short: "Record the quality of files already in Google Photos",
		Long: "Records whether uploaded files are stored as original or storage-saver. Google Photos does not report this, so gpsync relies on what you record here. gpsync reupload uses it to replace storage-saver copies.\n" +
			"\n" +
			"Applies to every uploaded file, or only those under the given folders.\n" +
			"\n" +
			"Example:\n" +
			"  gpsync quality storage-saver \"C:\\Photos\\2021\"",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q, err := parseRemoteQuality(args[0])
			if err != nil {
				return err
			}
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			n, err := db.SetRemoteQuality(q, args[1:])
			if err != nil {
				return err
			}
			fmt.Printf("Recorded %d already-uploaded file(s) as %s quality.\n", n, args[0])
			counts, err := db.RemoteQualityCounts()
			if err == nil {
				fmt.Printf("  %s\n", colDim(fmt.Sprintf("ledger now: %d original, %d storage-saver, %d unrecorded",
					counts[statedb.QualityOriginal], counts[statedb.QualityStorageSaver], counts[""])))
			}
			return nil
		},
	}
	return cmd
}

// doctorCmd checks every ledger invariant this project has had a real bug
// against, in one pass, instead of the bespoke one-off fix each of them
// previously needed. Read-only by default -- `--fix` is opt-in and only
// ever touches the unambiguous repairs (see engine.Finding.Fixable);
// anything needing a judgement call, or a command with richer options of
// its own, is reported with a pointer instead of silently acted on.
// verifyCmd is the one check a tool that claims a file is safely stored
// never did: confirming that files the ledger calls "uploaded" actually
// still exist in Google Photos. Until now "uploaded" meant only that an
// API call once returned 200 -- and this project has a concrete
// demonstration of why that isn't the same thing (16,222 album links sat
// recorded as "pending retry" while permanently broken, unnoticed for
// months, because nothing ever checked).
//
// Samples rather than sweeping: one exact mediaItems.get per file against
// an id already in the ledger, least-recently-verified first so repeated
// runs rotate through the library. Cheap enough to run daily -- a 50-item
// sample is 0.5% of the 10,000/day quota, and these are READS, not the
// concurrent-write budget the circuit breaker exists to protect.
// reuploadCmd re-queues files that were registered as synced WITHOUT
// gpsync ever uploading them -- `mark-synced` rows and the rclone-era
// backlog, identified by google_media_item_id IS NULL. Those are exactly
// the files whose remote quality is unknown, and re-sending the local
// original makes Google upgrade the stored copy in place rather than
// duplicating it. That observed merge behaviour is the whole reason this
// project deliberately never reconciles against the API, and
// this is the command that finally acts on it.
//
// Dry-run by default, matching recheck/fix-dates/clean-originals: it can
// queue a very large amount of upload work, which should never happen as
// a side effect of typing the command to see what it would do.
func reuploadCmd() *cobra.Command {
	var commit bool
	cmd := &cobra.Command{
		Use:   "reupload [folders...]",
		Short: "Queue storage-saver files to be replaced by originals",
		Long: "Queues every uploaded file recorded as storage-saver (see gpsync quality) for upload again. Google Photos replaces the existing copy instead of adding a second one.\n" +
			"\n" +
			"Shows what would be queued unless --commit is given. Pass folders to limit it.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync reupload\n" +
			"  gpsync reupload \"C:\\Photos\\2021\" --commit",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			rows, err := db.MarkedSyncedUnder(args)
			if err != nil {
				return err
			}
			fmt.Println(colHeader("Re-upload candidates:"))
			if len(rows) == 0 {
				fmt.Println("  none — no files are recorded as storage-saver quality")
				counts, cerr := db.RemoteQualityCounts()
				if cerr == nil {
					fmt.Printf("  %s\n", colDim(fmt.Sprintf("ledger: %d original, %d storage-saver, %d unrecorded",
						counts[statedb.QualityOriginal], counts[statedb.QualityStorageSaver], counts[""])))
					if counts[""] > 0 {
						fmt.Printf("  %s\n", colDim("Use `gpsync quality original|storage-saver [folders...]` to record what the unrecorded ones are."))
					}
				}
				return nil
			}

			// A file that has since been deleted locally can't be
			// re-uploaded; queuing it would only produce a permanent
			// failure on the next run.
			var queue []string
			var queueBytes, missingBytes int64
			var missing int
			for _, r := range rows {
				if r.FirstSourcePath == "" {
					continue
				}
				if _, statErr := os.Stat(r.FirstSourcePath); statErr != nil {
					missing++
					missingBytes += r.Size
					continue
				}
				queue = append(queue, r.SHA256)
				queueBytes += r.Size
			}

			fmt.Printf("  %d file(s), %s recorded as storage-saver quality\n", len(rows), humanBytes(queueBytes+missingBytes))

			// Same guard as `gpsync doctor`'s missing-source check, and for
			// the same reason: when essentially EVERY file looks absent,
			// the library is unreachable (drive not mounted, share offline,
			// or Windows paths being checked from WSL) rather than 15,000
			// files having been deleted. Without this the command reports
			// "0 can be re-uploaded", which reads as "nothing to do" and
			// silently hides the entire job.
			if missing > 0 && len(queue) == 0 && missing >= 20 {
				fmt.Printf("  %s\n", colWarn(fmt.Sprintf("all %d appear to be missing locally", missing)))
				fmt.Printf("  %s\n", colDim("That almost certainly means the library isn't reachable from here (drive not mounted,"))
				fmt.Printf("  %s\n", colDim("share offline, or Windows paths being checked from WSL) rather than the files being gone."))
				fmt.Printf("  %s\n", colDim("Re-run where the library actually lives — nothing has been queued."))
				return nil
			}
			if missing > 0 {
				fmt.Printf("  %s\n", colWarn(fmt.Sprintf("%d of them (%s) no longer exist locally and cannot be re-uploaded", missing, humanBytes(missingBytes))))
			}
			fmt.Printf("  %d file(s), %s can be re-uploaded\n", len(queue), humanBytes(queueBytes))

			if !commit {
				fmt.Printf("\n  %s\n", colDim("Dry run — nothing was changed. Re-run with --commit to queue these."))
				fmt.Printf("  %s\n", colDim("They will upload at whatever rate the throttle allows; see `gpsync info` for a projection."))
				return nil
			}
			n, err := db.RequeueForReupload(queue)
			if err != nil {
				return err
			}
			fmt.Printf("\n  %s\n", colOK(fmt.Sprintf("queued %d file(s) for re-upload", n)))
			fmt.Printf("  %s\n", colDim("gpsync-tray (or the next `gpsync sync`) will send the originals; Google merges them in place."))
			return nil
		},
	}
	cmd.Flags().BoolVar(&commit, "commit", false, "Queue the files")
	return cmd
}
