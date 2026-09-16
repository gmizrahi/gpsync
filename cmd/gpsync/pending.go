package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gmizrahi/gpsync/internal/engine"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

// printPendingFolders renders the pending backlog grouped by folder and
// returns the totals. Split from the cobra command so the output shape is
// testable without a terminal.
//
// Folder order and within-folder file order both come straight from
// pendingFolders (which sorts folders, and preserves ListPendingUnder's
// ORDER BY first_source_path within each) -- deliberately no re-sorting
// here, so this listing matches the order `gpsync upload` will actually work
// through them in.
func printPendingFolders(folders []engine.PendingFolder, summaryOnly bool) (files int, bytes int64) {
	for i, f := range folders {
		if i > 0 && !summaryOnly {
			fmt.Println()
		}
		fmt.Printf("%s %s\n", colHeader(f.Path),
			colDim(fmt.Sprintf("(%d files, %s)", f.Files, humanBytes(f.Bytes))))
		if !summaryOnly {
			for _, row := range f.Rows {
				fmt.Printf("  %-45s %s\n", filepath.Base(row.FirstSourcePath), colDim("("+humanBytes(row.Size)+")"))
			}
		}
		files += f.Files
		bytes += f.Bytes
	}
	return files, bytes
}

func pendingCmd() *cobra.Command {
	var summaryOnly bool
	cmd := &cobra.Command{
		Use:   "pending [folders...]",
		Short: "List files waiting to be uploaded, by folder",
		Long: "Lists pending files grouped by folder, in upload order: the whole ledger, or only the given folders.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync pending --summary\n" +
			"  gpsync pending \"C:\\Photos\\2026\"",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			folders, err := engine.PendingFolders(db, args)
			if err != nil {
				return err
			}
			if len(folders) == 0 {
				if len(args) > 0 {
					fmt.Printf("Nothing pending under: %s\n", strings.Join(args, ", "))
				} else {
					fmt.Println("Nothing pending — everything scanned has been uploaded.")
				}
				return nil
			}

			files, bytes := printPendingFolders(folders, summaryOnly)
			fmt.Println()
			fmt.Printf("%s pending files across %d folder(s), %s total.\n",
				colOK(fmt.Sprintf("%d", files)), len(folders), humanBytes(bytes))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&summaryOnly, "summary", "s", false, "Per-folder counts and totals only")
	return cmd
}

// ── auto-clean newly-excluded records ─────────────────────────────────
