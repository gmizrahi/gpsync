package main

import (
	"fmt"
	"path/filepath"

	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

// autoCleanExcluded silently drops any 'pending' or 'failed_permanent'
// ledger row whose source path is now excluded (a known-unsupported
// extension, or an ignored file/directory name) -- purely a path-based
// check against the CURRENT classification (built-in tables plus whatever
// config.toml's extra_* lists just added), no hashing or network involved.
//
// Called at the top of scan/upload/sync so adding an extension to
// extra_unsupported_extensions (or an entry to extra_ignored_file_names/
// extra_ignored_dir_names) takes effect immediately on the very next run,
// the same way `gpsync recheck --unsupported` does manually -- without
// that extra manual step. recheckCmd doesn't call this: its own
// --unsupported already covers failed_permanent rows with richer
// per-row reporting, and duplicating the same removal there would just be
// two ways to do the same thing.
func autoCleanExcluded(db *statedb.DB) (int, error) {
	pending, err := db.ListPending()
	if err != nil {
		return 0, err
	}
	failed, err := db.ListFailures(true) // permanentOnly
	if err != nil {
		return 0, err
	}
	var toRemove []string
	for _, row := range pending {
		if isNowExcluded(row.FirstSourcePath) {
			toRemove = append(toRemove, row.SHA256)
		}
	}
	for _, row := range failed {
		if isNowExcluded(row.FirstSourcePath) {
			toRemove = append(toRemove, row.SHA256)
		}
	}
	for _, sha := range toRemove {
		if err := db.DeleteUpload(sha); err != nil {
			return len(toRemove), err
		}
	}
	return len(toRemove), nil
}

// isNowExcluded reports whether path would be gated out of a scan today --
// a known-unsupported extension, an ignored file name, or under an ignored
// directory name anywhere in its path. Walks up the directory tree since
// an ignored directory (built-in or user-configured) prunes its WHOLE
// subtree, not just files directly inside it.
func isNowExcluded(path string) bool {
	if extensions.Classify(path) == extensions.Unsupported {
		return true
	}
	if scanner.IsIgnoredFileName(filepath.Base(path)) {
		return true
	}
	for dir := filepath.Dir(path); ; {
		if scanner.IsIgnoredDirName(filepath.Base(dir)) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// reportAutoClean prints a one-line notice only when autoCleanExcluded
// actually removed something -- silent (not a "0 cleaned" line) on every
// ordinary run where there's nothing new to clean, which is the common case.
func reportAutoClean(db *statedb.DB) {
	n, err := autoCleanExcluded(db)
	if err != nil {
		printDBError("auto-cleaning newly-excluded ledger records", err)
		return
	}
	if n > 0 {
		fmt.Printf("%s %d ledger record(s) removed for now-unsupported/ignored extensions.\n", colWarn("cleaned:"), n)
	}
}

// ── recheck ──────────────────────────────────────────────────────────
