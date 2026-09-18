package main

import (
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/engine"
)

// An empty ledger must say so rather than printing a wall of zeroes that
// looks like a broken report.
func TestPrintStats_EmptyLedgerSaysSo(t *testing.T) {
	out := captureStdout(t, func() { printStats(engine.Statistics{}) })
	if !strings.Contains(out, "Nothing tracked yet") {
		t.Errorf("output = %q, want it to name the empty case", out)
	}
}

// Uploaded and outstanding are the two real segments; a file that is
// neither (failed_permanent, needs_review) must be reported rather than
// folded into one of them, or the numbers quietly stop adding up.
func TestPrintStats_ReportsFilesThatAreNeitherUploadedNorPending(t *testing.T) {
	s := engine.Statistics{
		TotalFiles: 10,
		TotalBytes: 1000,
		ByKind: []engine.KindStat{
			{Label: "Photos", Count: 10, Bytes: 1000, UploadedCount: 6, PendingCount: 3},
		},
	}
	out := captureStdout(t, func() { printStats(s) })
	if !strings.Contains(out, "6 uploaded, 3 outstanding") {
		t.Errorf("output = %q, want both segments", out)
	}
	if !strings.Contains(out, "1 neither") {
		t.Errorf("output = %q, want the leftover file reported", out)
	}
}

// With everything accounted for, the "neither" note must not appear.
func TestPrintStats_NoLeftoverNoteWhenEverythingIsAccountedFor(t *testing.T) {
	s := engine.Statistics{
		TotalFiles: 5,
		ByKind: []engine.KindStat{
			{Label: "Photos", Count: 5, UploadedCount: 5, PendingCount: 0},
		},
	}
	out := captureStdout(t, func() { printStats(s) })
	if strings.Contains(out, "neither") {
		t.Errorf("output = %q, want no leftover note", out)
	}
}
