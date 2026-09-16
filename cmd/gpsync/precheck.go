package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/hashing"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// precheckResult tallies one precheckFolder run. Split out from the cobra
// command so the logic is testable without a terminal.
type precheckResult struct {
	newFiles  int
	newBytes  int64
	dupeFiles int
	dupeBytes int64
}

// precheckFolder walks src (recursively) hashing every file and looking it
// up in the ledger by content -- never writing to the ledger itself, this
// is a read-only check against what gpsync already knows is synced.
//
// A match means this exact content already exists somewhere the ledger
// knows about (any status, not just "uploaded" -- a pending/failed row
// still means the content lives in a scanned source folder already, so
// copying another one in is redundant either way). Non-matches are left
// alone in place; matches are moved into dupesDir so they never make it
// into the folder you're about to copy from src into your library.
//
// One safety exception: if the match's own source path IS this file (case-
// insensitively, matching the ledger's own path comparison convention),
// this file isn't a duplicate -- it's the canonical backed-up copy itself,
// e.g. src is pointed at (or overlaps) the real library by mistake. Moving
// it would delete the original from the library, not clear out a
// redundant copy, so it's left alone and reported separately.
func precheckFolder(db *statedb.DB, src, dupesDir string, dryRun bool, out io.Writer) (precheckResult, error) {
	var res precheckResult
	dupesDirClean := filepath.Clean(dupesDir)

	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.EqualFold(filepath.Clean(path), dupesDirClean) {
				return filepath.SkipDir
			}
			if scanner.IsIgnoredDirName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if scanner.IsIgnoredFileName(d.Name()) {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		sum, err := hashing.SHA256File(path)
		if err != nil {
			return fmt.Errorf("hashing %s: %w", path, err)
		}
		row, err := db.GetUpload(sum)
		if err != nil {
			return err
		}
		if row == nil {
			res.newFiles++
			res.newBytes += info.Size()
			return nil
		}
		if strings.EqualFold(filepath.Clean(path), filepath.Clean(row.FirstSourcePath)) {
			fmt.Fprintf(out, "  %s\n    %s\n", path, colDim("this IS the backed-up copy — left alone"))
			return nil
		}

		res.dupeFiles++
		res.dupeBytes += info.Size()
		fmt.Fprintf(out, "  %s\n    already synced as %s (%s)\n", path, row.FirstSourcePath, statusLabel(row.Status))
		if dryRun {
			fmt.Fprintf(out, "    %s\n", colDim("would move to "+dupesDir))
			return nil
		}
		// Reuses the same flat-by-basename, "_N" collision-suffixed move
		// `gpsync duplicates resolve` uses for its trash folder -- same
		// mechanism (move not delete, never overwrite), different
		// destination.
		dest, err := moveToTrash(dupesDir, path)
		if err != nil {
			return fmt.Errorf("moving %s: %w", path, err)
		}
		fmt.Fprintf(out, "    %s %s\n", colWarn("moved to"), dest)
		return nil
	})
	return res, err
}

func precheckCmd() *cobra.Command {
	var dryRun bool
	var dupesDirFlag string
	cmd := &cobra.Command{
		Use:   "precheck <folder>",
		Short: "Set aside files already synced before importing a folder",
		Long: "Hashes every file under <folder> and moves those already synced into the dupes folder, leaving only new files. Run it on a phone backup before copying it into your library. Nothing is added to the ledger.\n" +
			"\n" +
			"Moved files keep their names; a name that is already taken gets a suffix, such as photo_1.jpg. Set the dupes folder once with --dupes-dir; it must be outside every source folder.\n" +
			"\n" +
			"Examples:\n" +
			"  gpsync precheck \"C:\\Phone backup\" --dupes-dir \"C:\\Dupes\"\n" +
			"  gpsync precheck \"C:\\Phone backup\" --dry-run",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if dupesDirFlag != "" {
				cfg.DupesDir = dupesDirFlag
				if err := config.Save(cfg); err != nil {
					return fmt.Errorf("saving the dupes folder setting: %w", err)
				}
				fmt.Printf("Dupes folder set to %s (remembered for next time).\n", cfg.DupesDir)
			}
			if cfg.DupesDir == "" {
				return fmt.Errorf("no dupes folder configured — set one once with:\n" +
					"    gpsync precheck <folder> --dupes-dir \"C:\\Dupes\"\n" +
					"Files already synced are moved there rather than deleted. " +
					"Pick a location outside any folder gpsync scans")
			}

			src := args[0]
			info, err := os.Stat(src)
			if err != nil {
				return fmt.Errorf("cannot read %s: %w", src, err)
			}
			if !info.IsDir() {
				return fmt.Errorf("%s is a file — pass a folder", src)
			}

			if !dryRun {
				if err := os.MkdirAll(cfg.DupesDir, 0o755); err != nil {
					return fmt.Errorf("creating dupes folder %s: %w", cfg.DupesDir, err)
				}
			}

			db, err := statedb.Open()
			if err != nil {
				return err
			}
			defer db.Close()

			if dryRun {
				fmt.Println(colWarn("--dry-run: reporting what would move, nothing will actually be moved."))
			}
			fmt.Printf("Dupes folder: %s\n", cfg.DupesDir)
			fmt.Printf("Checking %s against the ledger...\n", src)

			res, err := precheckFolder(db, src, cfg.DupesDir, dryRun, os.Stdout)
			if err != nil {
				return err
			}

			fmt.Println()
			verb := "Moved"
			if dryRun {
				verb = "Would move"
			}
			fmt.Printf("%s new (left in place, %s). %s %s already-backed-up file(s) (%s) to the dupes folder.\n",
				colOK(fmt.Sprintf("%d", res.newFiles)), humanBytes(res.newBytes),
				verb, colWarn(fmt.Sprintf("%d", res.dupeFiles)), humanBytes(res.dupeBytes))
			return nil
		},
	}
	cmd.Flags().StringVar(&dupesDirFlag, "dupes-dir", "", "Folder to move duplicates into (saved as the default)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would move without moving anything")
	return cmd
}
