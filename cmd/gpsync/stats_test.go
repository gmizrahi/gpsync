package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// `gpsync stats --api` prints what the Statistics card shows (#76), and
// says so plainly when there are no tokens.
func TestPrintAPIUsage(t *testing.T) {
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	if out := captureStdout(t, func() { printAPIUsage(engine.APIUsageReport{Days: 30}, false, now) }); !strings.Contains(out, "No API tokens") {
		t.Errorf("no tokens: %q", out)
	}
	r := engine.APIUsageReport{
		Days: 7, Requests: 12, Bytes: 3 << 20, Refused: 2, LastRequest: now.Add(-2 * time.Hour),
		Tokens:   []engine.APITokenUsage{{Name: "trip-app", Requests: 10, LastUsed: now.Add(-2 * time.Hour), ExpiresAt: now.Add(41 * 24 * time.Hour), Stored: true}},
		Served:   []engine.APIServedRow{{Label: "Thumbnails", Requests: 9, Bytes: 3 << 20}, {Label: "from the cache", Requests: 8, Bytes: 2 << 20, Sub: true}},
		Refusals: []engine.APIRefusal{{Status: "401", Reason: "Bad, expired or revoked token", Requests: 2}},
		CacheDir: `D:\Pictures\.thumbnails`, CacheBytes: 600 << 20, CacheMax: 1 << 30,
	}
	out := captureStdout(t, func() { printAPIUsage(r, true, now) })
	for _, want := range []string{"last 7 days", "trip-app", "2 hours ago", "in 41 days", "Thumbnails", "    from the cache", "Bad, expired or revoked token (401)", `D:\Pictures\.thumbnails: 600.0MB of 1.0GB`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// --days takes only the periods Statistics offers; anything else is an
// error, not a silent 30.
func TestStatsAPI_RefusesAnUnofferedPeriod(t *testing.T) {
	dir := isolateState(t)
	cmd := statsCmd()
	cmd.SetArgs([]string{"--api", "--days", "14"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "7, 30 or 90") {
		t.Errorf("--days 14: %v; want it refused", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.sqlite")); err == nil {
		t.Error("a refused --days still opened the ledger")
	}
}
