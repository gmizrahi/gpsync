package main

import (
	"fmt"
	"time"

	"github.com/gmizrahi/gpsync/internal/auth"
	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

func verifyCmd() *cobra.Command {
	var sample int
	var force bool
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check that uploaded files still exist in Google Photos",
		Long: "Looks up a sample of uploaded files in Google Photos by media item ID, least recently checked first, so repeated runs cover the whole library. A file reported missing was deleted in Google Photos or never stored. Network and permission errors are reported as inconclusive.\n" +
			"\n" +
			"Example:\n" +
			"  gpsync verify --sample 200",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			// Refuse up front rather than firing requests that cannot
			// succeed. This build requests only photoslibrary.appendonly
			// (write-only), and Google answers reads on an unreadable item
			// with 404 -- so without this check every run reports the whole
			// sample as MISSING BACKUPS, which is exactly what the first
			// two releases of this command did.
			if !auth.HasReadScope() && !force {
				fmt.Println(colHeader("Verification:"))
				fmt.Printf("  %s\n", colWarn("gpsync cannot read from Google Photos, so verification isn't possible."))
				fmt.Printf("  %s\n", colDim("It requests only photoslibrary.appendonly (write-only) — deliberately, since uploading"))
				fmt.Printf("  %s\n", colDim("never needs read access. Google answers reads on an unreadable item with 404, so every"))
				fmt.Printf("  %s\n", colDim("check would come back looking like a missing file whether or not it is really there."))
				fmt.Printf("  %s\n", colDim("Enabling this would need a read scope and a fresh consent (`gpsync setup`)."))
				fmt.Printf("  %s\n", colDim("If your token came from `gpsync import-rclone` it may already carry broader scopes —"))
				fmt.Printf("  %s\n", colDim("token.json doesn't record them, so `--force` will try anyway."))
				if n, cerr := db.ClearVerificationVerdicts(); cerr == nil && n > 0 {
					fmt.Printf("  %s\n", colDim(fmt.Sprintf("Cleared %d verification result(s) recorded before this was understood.", n)))
				}
				return nil
			}

			client, err := auth.GetHTTPClient(cmd.Context())
			if err != nil {
				return err
			}
			res, err := engine.VerifySample(client, db, sample, time.Now())
			if err != nil {
				return err
			}

			fmt.Println(colHeader("Verification:"))
			if res.AccessProblem {
				// Everything came back 404. Almost certainly means gpsync
				// cannot READ from Google at all rather than that the
				// library is gone -- see VerifySample's guard.
				fmt.Printf("  %s all %d sampled file(s) came back \"not found\"\n", colWarn("!"), res.Checked)
				fmt.Printf("  %s\n", colDim("This is an access problem, not evidence your backups are missing. gpsync requests only"))
				fmt.Printf("  %s\n", colDim("photoslibrary.appendonly (write-only), and Google answers reads on an unreadable item"))
				fmt.Printf("  %s\n", colDim("with 404 rather than 403 -- so every check fails identically whether or not the file"))
				fmt.Printf("  %s\n", colDim("is there. Verifying would need a read scope and a fresh consent; see `gpsync setup`."))
				if n, cerr := db.ClearVerificationVerdicts(); cerr == nil && n > 0 {
					fmt.Printf("  %s\n", colDim(fmt.Sprintf("Cleared %d earlier verification result(s) that were recorded on this bad assumption.", n)))
				}
				return nil
			}
			if res.Checked == 0 {
				fmt.Println("  nothing to verify yet — no files have been uploaded by gpsync")
				return nil
			}
			fmt.Printf("  checked %d file(s): %s confirmed", res.Checked, colOK(fmt.Sprintf("%d", res.OK)))
			if res.Inconclusive > 0 {
				fmt.Printf(", %s inconclusive", colWarn(fmt.Sprintf("%d", res.Inconclusive)))
			}
			if len(res.Missing) > 0 {
				fmt.Printf(", %s MISSING", colErr(fmt.Sprintf("%d", len(res.Missing))))
			}
			fmt.Println()
			for _, m := range res.Missing {
				fmt.Printf("  %s %s\n      %s\n", colErr("missing:"), m.Path, colDim(m.Reason))
			}

			verified, failed, total, lastAt, err := db.VerificationSummary()
			if err == nil && total > 0 {
				fmt.Printf("  %s\n", colDim(fmt.Sprintf("%d of %d uploaded file(s) have ever been verified (%.1f%%), %d with a recorded problem",
					verified, total, float64(verified)/float64(total)*100, failed)))
				if lastAt > 0 {
					fmt.Printf("  %s\n", colDim("last verified: "+time.Unix(int64(lastAt), 0).Format("2006-01-02 15:04:05")))
				}
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&sample, "sample", 50, "Number of files to check")
	cmd.Flags().BoolVar(&force, "force", false, "Try even without read access, e.g. with an rclone-imported token")
	return cmd
}
