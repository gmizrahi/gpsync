package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/fatih/color"
)

// Themed print helpers. fatih/color auto-detects Windows terminal support
// and disables escape codes entirely when stdout isn't a real terminal
// (piped/redirected), so output stays clean in logs either way.
var (
	colHeader = color.New(color.FgCyan, color.Bold).SprintFunc() // folder/step headers
	colBatch  = color.New(color.FgBlue, color.Bold).SprintFunc() // whole-batch totals block, kept visually distinct from the current folder's own block
	colDim    = color.New(color.Faint).SprintFunc()              // elapsed time, secondary detail, rule lines
	colOK     = color.New(color.FgGreen).SprintFunc()            // success counts, checkmarks
	colWarn   = color.New(color.FgYellow).SprintFunc()           // retryable failures, quota warnings
	colErr    = color.New(color.FgRed).SprintFunc()              // permanent failures, error text
	colFile   = color.New(color.FgWhite).SprintFunc()            // current filename in progress lines
	checkMark = color.GreenString("✓")
	crossMark = color.RedString("✗")
)

// separatorWidth is the rule line's length in printFolderHeader.
const separatorWidth = 64

// printFolderHeader marks a folder-change in a multi-folder run (`gpsync
// sync`/`gpsync mark-synced`) with a dim rule line above the folder name --
// meant to be visually easy to spot scrolling past, without shouting the
// way a bare "=== ... ===" line does.
func printFolderHeader(idx, total int, folder string) {
	fmt.Println(colDim(strings.Repeat("─", separatorWidth)))
	fmt.Println(colHeader(fmt.Sprintf("[%d/%d] %s", idx, total, folder)))
}

// printSkip is the onSkip callback shared by scan/sync/mark-synced: prints
// each skipped file/folder live, as it's encountered, not just a summary count.
func printSkip(path, reason string) {
	fmt.Printf("  %s %s (%s)\n", colWarn("skip:"), path, colDim(reason))
}

// printDBError is the onDBError callback for the upload pipeline: a write
// to the local ledger failed. These used to be discarded entirely, which is
// how a full disk or a damaged state.sqlite could quietly turn into "gpsync
// re-uploads files it already uploaded" with nothing on screen to explain
// it. Warning-colored rather than error-colored: the upload itself may well
// have succeeded -- it's the record of it that didn't.
func printDBError(context string, err error) {
	fmt.Printf("  %s %s: %s\n", colWarn("ledger write failed:"), context, colErr(err.Error()))
}

// formatInFlight renders the set of files currently being hashed (there can
// be more than one under concurrency > 1) as a short comma-joined list of
// basenames, instead of showing only the single most-recently-finished file.
func formatInFlight(files []string) string {
	if len(files) == 0 {
		return "-"
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = filepath.Base(f)
	}
	return strings.Join(names, ", ")
}

// printPreScan is the onPreScan callback shared by scan/sync/mark-synced:
// shows what's already known about a folder before hashing starts, not
// only in the final "done" summary.
func printPreScan(total, alreadyKnown, toHashNow int) {
	if alreadyKnown > 0 {
		fmt.Printf("  %d files found — %s already known, checking %s new/changed\n",
			total, colDim(fmt.Sprintf("%d", alreadyKnown)), colOK(fmt.Sprintf("%d", toHashNow)))
	} else {
		fmt.Printf("  %d files found, all new\n", total)
	}
}

// eol clears from the cursor to the end of the line -- for \r-updated
// progress lines, so a shorter line (e.g. a shorter filename) doesn't leave
// trailing characters from a longer previous line still on screen. Only
// emitted when colors are (i.e. we're on a real terminal); on a redirected/
// piped stdout this is skipped so no raw escape bytes end up in log files.
func eol() string {
	if color.NoColor {
		return ""
	}
	return "\x1b[K"
}
