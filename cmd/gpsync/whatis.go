package main

import (
	"fmt"
	"os"
	"time"

	"github.com/gmizrahi/gpsync/internal/hashing"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/spf13/cobra"
)

// whatisFile hashes a local file and reports what the ledger knows about
// that exact content. Split from the cobra command so it is testable.
//
// Dedup identity in this project is content SHA-256, not path, which is
// what makes this work at all: a file downloaded back out of Google Photos
// under some generated name still hashes to the same value as the original
// on disk, so the ledger can name the local file it came from.
func whatisFile(db *statedb.DB, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory — pass a single file", path)
	}

	sum, err := hashing.SHA256File(path)
	if err != nil {
		return fmt.Errorf("hashing %s: %w", path, err)
	}
	fmt.Printf("%s %s\n", colHeader("File:"), path)
	fmt.Printf("%s %s  %s\n", colHeader("SHA-256:"), sum, colDim("("+humanBytes(info.Size())+")"))

	row, err := db.GetUpload(sum)
	if err != nil {
		return err
	}
	if row == nil {
		fmt.Printf("%s %s\n", colHeader("Ledger:"), colWarn("not found — gpsync has never seen this content"))
		fmt.Println(colDim("  If you expected a match, this content differs from anything scanned —"))
		fmt.Println(colDim("  e.g. Google Photos re-encoded it on download, so the bytes are no longer identical."))
		return nil
	}

	fmt.Printf("%s %s\n", colHeader("Ledger:"), colOK("found"))
	fmt.Printf("  %s %s\n", colHeader("Original file:"), row.FirstSourcePath)
	fmt.Printf("  %s %s\n", colHeader("Status:"), statusLabel(row.Status))
	if row.GoogleMediaItemID.Valid && row.GoogleMediaItemID.String != "" {
		fmt.Printf("  %s %s\n", colHeader("Google media item:"), colDim(row.GoogleMediaItemID.String))
	} else if row.Status == "uploaded" {
		fmt.Printf("  %s\n", colDim("marked synced via `gpsync mark-synced` — never uploaded by gpsync, so there's no media item id"))
	}
	if row.UploadedAt.Valid && row.UploadedAt.Float64 > 0 {
		fmt.Printf("  %s %s\n", colHeader("Uploaded at:"), time.Unix(int64(row.UploadedAt.Float64), 0).Format("2006-01-02 15:04:05"))
	}
	if row.CapturedAt.Valid && row.CapturedAt.Float64 > 0 {
		fmt.Printf("  %s %s\n", colHeader("Captured at:"), time.Unix(int64(row.CapturedAt.Float64), 0).Format("2006-01-02 15:04:05"))
	}
	if row.LastErrorMessage.Valid && row.LastErrorMessage.String != "" {
		fmt.Printf("  %s %s\n", colHeader("Last error:"), colErr(row.LastErrorMessage.String))
	}

	// Every other local path with this same content.
	if groups, gerr := db.DuplicateGroups(); gerr == nil {
		for _, g := range groups {
			if g.SHA256 != sum {
				continue
			}
			fmt.Printf("  %s\n", colHeader("Also on disk at:"))
			for _, other := range g.Paths {
				if other != row.FirstSourcePath {
					fmt.Printf("    %s\n", other)
				}
			}
		}
	}
	return nil
}

func statusLabel(status string) string {
	switch status {
	case "uploaded":
		return colOK(status)
	case "pending":
		return colWarn(status)
	case "failed_permanent", "failed_retryable":
		return colErr(status)
	}
	return status
}

func whatisCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whatis <file>",
		Short: "Identify a file by its content",
		Long: "Hashes the file and shows which tracked file has the same content and what gpsync did with it; useful for a photo downloaded from Google Photos. Renamed copies match; copies whose bytes changed do not. Changes nothing.\n" +
			"\n" +
			"Example:\n" +
			"  gpsync whatis \"C:\\Users\\me\\Downloads\\IMG_1234.jpg\"",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()
			return whatisFile(db, args[0])
		},
	}
}

// ── pending ────────────────────────────────────────────────────────────
