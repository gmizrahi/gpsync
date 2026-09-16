package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/fatih/color"

	"github.com/gmizrahi/gpsync/internal/uploader"
)

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0B"},
		{500, "500B"},
		{1024, "1.0KB"},
		{1536, "1.5KB"},
		{1024 * 1024, "1.0MB"},
		{1024 * 1024 * 1024, "1.0GB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.n); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestTruncateName(t *testing.T) {
	if got := truncateName("short.jpg", 40); got != "short.jpg" {
		t.Errorf("got %q, want unchanged", got)
	}
	long := "a_very_long_filename_that_exceeds_the_display_width.jpg"
	got := truncateName(long, 20)
	if len(got) != 20 {
		t.Errorf("truncateName length = %d, want 20", len(got))
	}
	if got[len(got)-3:] != "..." {
		t.Errorf("expected truncated name to end with '...', got %q", got)
	}
}

// TestBatchTracker_SeedsFromAlreadySynced_AndAccumulatesAcrossFolders
// proves the two things the "Totals for batch" section depends on: (1) a
// freshly-created tracker starts filesDone/bytesDone already at the
// pre-scan's "already synced" figures rather than 0, so the batch total
// doesn't misleadingly look stuck at the start of a run, and (2)
// recordCompleted (called from a dashboard's onProgress, once per folder)
// accumulates across every folder in the batch rather than resetting --
// simulating two folders' worth of uploads reported one after another,
// exactly as `gpsync sync` drives it (a fresh per-folder dashboard, one
// shared batchTracker).
func TestBatchTracker_SeedsFromAlreadySynced_AndAccumulatesAcrossFolders(t *testing.T) {
	b := newBatchTracker(10, 1000, 3, 300)

	s := b.snapshot()
	if s.filesDone != 3 || s.bytesDone != 300 {
		t.Fatalf("initial snapshot = filesDone=%d bytesDone=%d, want 3/300 (seeded from alreadySynced)", s.filesDone, s.bytesDone)
	}
	if s.filesTotal != 10 || s.bytesTotal != 1000 || s.alreadySynced != 3 {
		t.Fatalf("initial snapshot totals = %+v, want filesTotal=10 bytesTotal=1000 alreadySynced=3", s)
	}

	// Folder 1: two successful uploads.
	b.recordCompleted(true, 50)
	b.recordCompleted(true, 50)
	// Folder 2 (a separate dashboard instance in real use, same tracker):
	// one success, one permanent failure -- failures count toward
	// filesDone (they're "accounted for") but never bytesDone.
	b.recordCompleted(true, 100)
	b.recordCompleted(false, 999)

	s = b.snapshot()
	if s.filesDone != 3+4 {
		t.Errorf("filesDone = %d, want %d (3 seeded + 4 recorded across both folders)", s.filesDone, 3+4)
	}
	if s.bytesDone != 300+200 {
		t.Errorf("bytesDone = %d, want %d (300 seeded + 200 from the 3 successful uploads; the failure's 999 bytes must not count)", s.bytesDone, 300+200)
	}
}

// TestDashboard_OnThrottle_ClearsAndRedrawsInPlace proves the fix for a
// throttle/quota message getting silently erased: onThrottle must clear
// the currently-drawn block (via clearDrawn, which also resets
// linesDrawn to 0) before printing its own permanent line, then redraw
// the block again -- leaving linesDrawn reflecting the freshly-repainted
// block, not stuck at 0. A stale/wrong linesDrawn is exactly what made
// the previous plain-fmt.Printf version get scribbled over: the next
// repaint's cursor-up count didn't know a line had been inserted.
func TestDashboard_OnThrottle_ClearsAndRedrawsInPlace(t *testing.T) {
	orig := color.NoColor
	color.NoColor = false
	defer func() { color.NoColor = orig }()

	d := newDashboard("/some/folder", 1, 100, nil)
	d.onBytes(uploader.ByteProgressEvent{Path: "/a.jpg", Sent: 50, Total: 100})
	if d.linesDrawn == 0 {
		t.Fatal("setup: expected a live block to already be drawn before onThrottle fires")
	}

	d.onThrottle("slow down", 5.0, 2)

	if d.linesDrawn == 0 {
		t.Error("onThrottle left linesDrawn at 0 -- the live block was cleared but never redrawn, so the next repaint's cursor-up math would be wrong and could overwrite the throttle message itself")
	}
}

func TestDashboard_TracksBytesAndFilesAcrossEvents_NoColorMode(t *testing.T) {
	// Force the no-redraw code path so this test doesn't depend on a real
	// terminal, and doesn't leave escape codes in test output.
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	d := newDashboard("/mnt/c/Photos/2024/2024_03", 2, 300, nil)

	d.onBytes(uploader.ByteProgressEvent{Path: "/a.jpg", Sent: 50, Total: 100})
	d.onBytes(uploader.ByteProgressEvent{Path: "/a.jpg", Sent: 100, Total: 100})
	d.onProgress(uploader.ProgressEvent{Done: 1, Total: 2, LastFile: "/a.jpg", LastOK: true, LastSize: 100})

	d.mu.Lock()
	if d.bytesDone != 100 {
		t.Errorf("bytesDone = %d, want 100", d.bytesDone)
	}
	if d.filesDone != 1 || d.filesTotal != 2 {
		t.Errorf("filesDone/filesTotal = %d/%d, want 1/2", d.filesDone, d.filesTotal)
	}
	if _, stillInFlight := d.inFlight["/a.jpg"]; stillInFlight {
		t.Error("expected /a.jpg to be removed from inFlight once completed")
	}
	d.mu.Unlock()

	d.onBytes(uploader.ByteProgressEvent{Path: "/b.jpg", Sent: 30, Total: 200})
	d.mu.Lock()
	ft, ok := d.inFlight["/b.jpg"]
	if !ok || ft.sent != 30 || ft.total != 200 {
		t.Errorf("unexpected /b.jpg in-flight state: %+v (ok=%v)", ft, ok)
	}
	d.mu.Unlock()

	d.onProgress(uploader.ProgressEvent{Done: 2, Total: 2, LastFile: "/b.jpg", LastOK: false, LastSize: 200})
	d.mu.Lock()
	if d.bytesDone != 100 {
		t.Errorf("bytesDone after a failed file = %d, want 100 (failures don't count toward bytesDone)", d.bytesDone)
	}
	if d.filesDone != 2 {
		t.Errorf("filesDone = %d, want 2", d.filesDone)
	}
	d.mu.Unlock()

	d.finish() // must not panic
}

// TestDashboard_OnBytes_FinalChunkAlwaysForcesRedraw proves the fix for a
// file's 100% state never being visible: the final onBytes call (sent ==
// total) must repaint immediately, bypassing the throttle window, since
// onProgress removes the file from the list right after -- if that last
// chunk gets throttled away, the file jumps from some partial percent
// straight to gone, never showing 100%.
func TestDashboard_OnBytes_FinalChunkAlwaysForcesRedraw(t *testing.T) {
	orig := color.NoColor
	color.NoColor = false // exercise the real repaint path, not the no-op NoColor branch
	defer func() { color.NoColor = orig }()

	d := newDashboard("/some/folder", 1, 100, nil)
	d.onBytes(uploader.ByteProgressEvent{Path: "/a.jpg", Sent: 50, Total: 100})
	firstRedraw := d.lastRedrawAt

	// Immediately send the final chunk -- essentially no time has passed,
	// well inside redrawInterval, so a naive throttle check would skip it.
	d.onBytes(uploader.ByteProgressEvent{Path: "/a.jpg", Sent: 100, Total: 100})
	secondRedraw := d.lastRedrawAt

	if !secondRedraw.After(firstRedraw) {
		t.Error("expected the 100% chunk to force an immediate redraw, bypassing the throttle")
	}
}

// TestTruncateName_CutsByRuneNotByte proves the fix for mojibake in the
// in-flight file list: truncateName sliced by BYTE index, so a filename
// with any multi-byte character (accented European names, non-Latin
// scripts -- entirely normal in a real photo library) could be cut in the
// middle of a rune and printed as invalid UTF-8.
func TestTruncateName_CutsByRuneNotByte(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
	}{
		{"accented latin", "vacaciones_en_España_con_la_família_2024.jpg", 20},
		{"cyrillic", "Отпуск_в_Крыму_фотографии_2024.jpg", 15},
		{"japanese", "家族旅行の写真アルバム二〇二四年.jpg", 10},
		{"emoji", "trip🏖️🌴🍹_summer_photos_2024.jpg", 12},
		{"cut lands exactly on a multi-byte boundary", "ñññññññññññññ.jpg", 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateName(tc.in, tc.max)
			if !utf8.ValidString(got) {
				t.Fatalf("truncateName(%q, %d) = %q -- not valid UTF-8 (cut mid-rune)", tc.in, tc.max, got)
			}
			if n := utf8.RuneCountInString(got); n > tc.max {
				t.Errorf("truncateName(%q, %d) returned %d runes, want at most %d", tc.in, tc.max, n, tc.max)
			}
			if !strings.HasSuffix(got, "...") {
				t.Errorf("truncateName(%q, %d) = %q, want a '...' suffix on a truncated name", tc.in, tc.max, got)
			}
		})
	}

	// Short inputs and tiny widths must still behave, and never panic.
	if got := truncateName("ok.jpg", 40); got != "ok.jpg" {
		t.Errorf("got %q, want unchanged", got)
	}
	if got := truncateName("ñññññ", 2); utf8.RuneCountInString(got) != 2 || !utf8.ValidString(got) {
		t.Errorf("truncateName with max<=3 = %q, want 2 valid runes", got)
	}
	if got := truncateName("anything", 0); got != "" {
		t.Errorf("truncateName(_, 0) = %q, want empty", got)
	}
}

// TestDashboard_OnBytes_RetriedFileNeverShowsNegativeSpeed proves the fix
// for garbage like "-52428800B/s" in the live view. On a file's SECOND
// upload attempt the uploader's progressReader restarts its counter at 0,
// but the dashboard still held attempt 1's high-water mark in lastSent --
// so the next instantaneous rate was (small - large)/dt, a large negative
// number, which then smoothed into the displayed speed.
func TestDashboard_OnBytes_RetriedFileNeverShowsNegativeSpeed(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true // state-only; no escape codes in test output
	defer func() { color.NoColor = orig }()

	d := newDashboard("/some/folder", 1, 10_000_000, nil)
	const path = "/big-video.mp4"

	// Attempt 1 gets most of the way through.
	d.onBytes(uploader.ByteProgressEvent{Path: path, Sent: 5_000_000, Total: 10_000_000})
	time.Sleep(60 * time.Millisecond) // clear the dt > 0.05s guard
	d.onBytes(uploader.ByteProgressEvent{Path: path, Sent: 9_000_000, Total: 10_000_000})

	d.mu.Lock()
	ft := d.inFlight[path]
	if ft == nil || ft.lastSent == 0 {
		d.mu.Unlock()
		t.Fatal("setup: expected a high-water mark from the first attempt")
	}
	d.mu.Unlock()

	// The upload fails and is retried: the byte counter restarts at 0.
	d.onBytes(uploader.ByteProgressEvent{Path: path, Sent: 100_000, Total: 10_000_000})
	time.Sleep(60 * time.Millisecond)
	d.onBytes(uploader.ByteProgressEvent{Path: path, Sent: 400_000, Total: 10_000_000})

	d.mu.Lock()
	defer d.mu.Unlock()
	ft = d.inFlight[path]
	if ft == nil {
		t.Fatal("file disappeared from the in-flight list")
	}
	if ft.speed < 0 {
		t.Errorf("speed = %v after a retry restarted the byte counter -- the dashboard would print %q", ft.speed, humanBytes(int64(ft.speed))+"/s")
	}
	if ft.sent != 400_000 {
		t.Errorf("sent = %d, want 400000 (the retry's own progress)", ft.sent)
	}
}

// TestFormatCompletedLine_ShowsWhyAFileFailed proves the failure reason now
// reaches the permanent per-file log line. A run where 38 files failed
// back-to-back printed nothing but "✗ name failed (size)" -- the reason was
// recorded only in the ledger, so the user had to run `gpsync log` separately
// just to learn what went wrong.
func TestFormatCompletedLine_ShowsWhyAFileFailed(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true // no escape codes, so the assertions read the real text
	defer func() { color.NoColor = orig }()

	at := time.Date(2026, 8, 29, 14, 5, 9, 0, time.UTC)

	failed := formatCompletedLine(at, "/photos/2024/IMG_0001.jpg", false, 2048, "Quota exceeded for quota 'concurrent write request'")
	if !strings.Contains(failed, "IMG_0001.jpg") {
		t.Errorf("line %q does not name the file", failed)
	}
	if !strings.Contains(failed, "failed") {
		t.Errorf("line %q does not say the file failed", failed)
	}
	if !strings.Contains(failed, "concurrent write request") {
		t.Errorf("line %q does not carry the failure reason -- this is exactly what the user had to run `gpsync log` to find", failed)
	}

	ok := formatCompletedLine(at, "/photos/2024/IMG_0002.jpg", true, 2048, "")
	if strings.Contains(ok, "failed") {
		t.Errorf("success line %q should not mention failure", ok)
	}

	// A long, multi-line API message must stay on ONE tight line -- a
	// folder full of failures has to remain readable.
	long := "Failed:\tThere was an error while trying to create this media item.\nRequest identifier: " + strings.Repeat("x", 300)
	line := formatCompletedLine(at, "/photos/a.jpg", false, 10, long)
	if strings.ContainsAny(line, "\n\t") {
		t.Errorf("failure line contains a newline or tab: %q", line)
	}
	if !strings.Contains(line, "...") {
		t.Errorf("expected a long reason to be truncated with '...': %q", line)
	}
	if n := utf8.RuneCountInString(line); n > 200 {
		t.Errorf("failure line is %d runes long -- too long to stay readable across many failures: %q", n, line)
	}

	// A failure with no reason available must not print a dangling dash.
	bare := formatCompletedLine(at, "/photos/b.jpg", false, 10, "")
	if strings.Contains(bare, "—") {
		t.Errorf("line %q has a reason separator but no reason", bare)
	}
}

// TestDashboard_OnProgress_LogsTheFailureReason walks the same path the
// uploader actually drives, confirming ProgressEvent.LastErrorMessage is
// carried through onProgress into the permanent log line rather than
// dropped between the two.
func TestDashboard_OnProgress_LogsTheFailureReason(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = origStdout }()

	d := newDashboard("/photos/2024", 1, 100, nil)
	d.onProgress(uploader.ProgressEvent{
		Done: 1, Total: 1,
		LastFile: "/photos/2024/IMG_0007.jpg", LastOK: false, LastSize: 100,
		LastErrorMessage: "Quota exceeded for quota 'concurrent write request'",
	})

	w.Close()
	os.Stdout = origStdout
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "concurrent write request") {
		t.Errorf("dashboard output %q does not include the failure reason", out)
	}
}

// TestDashboard_OnDBError_ClearsAndRedrawsInPlace: the ledger-write warning
// must go through the same clearDrawn/redraw bookkeeping as onThrottle, or
// the very next repaint (a fraction of a second later) scribbles straight
// over it -- and a silently-lost ledger write is the last thing that should
// be invisible.
func TestDashboard_OnDBError_ClearsAndRedrawsInPlace(t *testing.T) {
	orig := color.NoColor
	color.NoColor = false
	defer func() { color.NoColor = orig }()

	d := newDashboard("/some/folder", 1, 100, nil)
	d.onBytes(uploader.ByteProgressEvent{Path: "/a.jpg", Sent: 50, Total: 100})
	if d.linesDrawn == 0 {
		t.Fatal("setup: expected a live block to already be drawn")
	}

	d.onDBError("marking /a.jpg uploaded", errors.New("disk full"))

	if d.linesDrawn == 0 {
		t.Error("onDBError left linesDrawn at 0 -- the live block was cleared but never redrawn, so the next repaint would overwrite the warning it just printed")
	}
}

// TestFormatCompletedLine_TightColumnsAndTrimmedReason covers the two
// readability fixes to the permanent log line. The filename column used to
// be padded to 45, leaving a chasm of whitespace after every normal-length
// photo name; and the reason cap of 80 cut Google's throttle message
// mid-clause, deep inside the boilerplate tail ("... of service
// 'photoslibrary.googleapis.com' for consumer ..."). The cap is a general
// one -- no per-message special-casing -- so this also checks it leaves
// other real message shapes reading sensibly.
func TestFormatCompletedLine_TightColumnsAndTrimmedReason(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	at := time.Date(2026, 8, 29, 14, 5, 9, 0, time.UTC)

	// A normal-length filename must not be followed by a whitespace gulf.
	line := formatCompletedLine(at, "/photos/2024/IMG_0001.jpg", true, 2048, "")
	// "IMG_0001.jpg" is 12 chars; the old 45-wide column left 33 spaces
	// after it. Anything approaching that is the gulf this fixed.
	const maxGap = 25
	if strings.Contains(line, strings.Repeat(" ", maxGap)) {
		t.Errorf("line has a run of %d+ spaces after a normal filename -- the name column is still too wide:\n%q", maxGap, line)
	}
	if !strings.Contains(line, "IMG_0001.jpg") {
		t.Errorf("line %q lost the filename", line)
	}

	// The real throttle message must trim right after the informative head,
	// before the service/consumer boilerplate.
	throttle := "Quota exceeded for quota 'concurrent write request' of service 'photoslibrary.googleapis.com' for consumer 'project_number:12345'."
	line = formatCompletedLine(at, "/photos/a.jpg", false, 10, throttle)
	if !strings.Contains(line, "Quota exceeded for quota 'concurrent write request'") {
		t.Errorf("the informative head of the throttle message was cut off:\n%q", line)
	}
	if strings.Contains(line, "of service") {
		t.Errorf("the boilerplate tail survived the trim -- the reason cap is still too generous:\n%q", line)
	}

	// Other real shapes must still read sensibly (not cut to uselessness).
	for _, msg := range []string{
		"The file is corrupt and cannot be processed",
		"Failed: There was an error while trying to create this media item.",
		"Quota exceeded for quota metric 'requests' and limit 'requests per day'",
	} {
		got := singleLine(msg, maxReasonLen)
		if utf8.RuneCountInString(got) > maxReasonLen {
			t.Errorf("singleLine(%q) = %q, longer than the cap", msg, got)
		}
		// At least the first few words survive -- enough to identify the problem.
		head := strings.Join(strings.Fields(msg)[:4], " ")
		if !strings.HasPrefix(got, head) {
			t.Errorf("singleLine(%q) = %q, want it to keep at least the leading %q", msg, got, head)
		}
	}

	// A very long filename must be truncated into the column, not push the
	// detail out of alignment.
	longName := "/photos/" + strings.Repeat("verylongname", 8) + ".jpg"
	short := formatCompletedLine(at, "/photos/a.jpg", true, 1, "")
	long := formatCompletedLine(at, longName, true, 1, "")
	if len(long) != len(short) {
		t.Errorf("a long filename changed the line width (%d vs %d) -- the detail column is no longer aligned:\n%q\n%q", len(long), len(short), long, short)
	}
}

// TestDashboard_DeferredFile_IsNotCountedAsDone proves the live view treats
// a throttle-deferred file honestly. Such a file has NOT finished: the
// circuit breaker set it aside and will retry it after its pause. It must
// leave the in-flight list (it is not transferring) but must not advance
// the file/byte counters, or it gets counted a second time when the retry
// succeeds and the batch totals overshoot.
func TestDashboard_DeferredFile_IsNotCountedAsDone(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	batch := newBatchTracker(2, 300, 0, 0)
	d := newDashboard("/photos/2024", 2, 300, batch)

	d.onBytes(uploader.ByteProgressEvent{Path: "/a.jpg", Sent: 60, Total: 100})
	d.onProgress(uploader.ProgressEvent{
		Done: 0, Total: 2, LastFile: "/a.jpg", LastOK: false, LastSize: 100,
		LastDeferred: true, LastErrorMessage: "Quota exceeded for quota 'concurrent write request'",
	})

	d.mu.Lock()
	if _, still := d.inFlight["/a.jpg"]; still {
		t.Error("a deferred file must leave the in-flight list -- it is not transferring any more")
	}
	if d.filesDone != 0 {
		t.Errorf("filesDone = %d, want 0 -- a file held for retry has not finished", d.filesDone)
	}
	if d.bytesDone != 0 {
		t.Errorf("bytesDone = %d, want 0", d.bytesDone)
	}
	d.mu.Unlock()
	if bs := batch.snapshot(); bs.filesDone != 0 {
		t.Errorf("batch filesDone = %d, want 0 -- counting a held file here double-counts it when the retry lands", bs.filesDone)
	}

	// The retry succeeds: NOW it counts, exactly once.
	d.onProgress(uploader.ProgressEvent{Done: 1, Total: 2, LastFile: "/a.jpg", LastOK: true, LastSize: 100})
	d.mu.Lock()
	if d.filesDone != 1 || d.bytesDone != 100 {
		t.Errorf("after the retry succeeded: filesDone=%d bytesDone=%d, want 1/100", d.filesDone, d.bytesDone)
	}
	d.mu.Unlock()
	if bs := batch.snapshot(); bs.filesDone != 1 {
		t.Errorf("batch filesDone = %d, want exactly 1", bs.filesDone)
	}
}

// TestFormatDeferredLine_ReadsAsHeldNotFailed: a deferred file's log line
// must not claim the file failed. Nothing is wrong with it; it just hasn't
// been retried yet, and telling the user it failed is something they would
// act on.
func TestFormatDeferredLine_ReadsAsHeldNotFailed(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	line := formatDeferredLine(time.Date(2026, 8, 29, 14, 5, 9, 0, time.UTC),
		"/photos/2024/IMG_0009.jpg", "Quota exceeded for quota 'concurrent write request'")
	if strings.Contains(line, "failed") {
		t.Errorf("deferred line claims the file failed: %q", line)
	}
	if !strings.Contains(line, "held for retry") {
		t.Errorf("deferred line does not say the file is held: %q", line)
	}
	if !strings.Contains(line, "IMG_0009.jpg") {
		t.Errorf("deferred line lost the filename: %q", line)
	}
}

func activeBackoff() uploader.BackoffStatus {
	return uploader.BackoffStatus{
		Active:         true,
		Reason:         "Quota exceeded for quota 'concurrent write request' of service 'photoslibrary.googleapis.com'",
		Rung:           3,
		TotalRungs:     7,
		RungWait:       3 * time.Minute,
		RemainingWait:  47 * time.Second,
		Concurrency:    1,
		MaxConcurrency: 6,
	}
}

// TestDashboard_BackoffSection_RendersOnlyWhilePaused proves the live
// circuit-breaker status is part of the redrawn block, not a one-shot line:
// it appears inside the batch totals while a pause is active and disappears
// the moment it is cleared. Without it, a pause of up to 15 minutes showed
// one static line and then a normally-redrawing dashboard underneath with
// no indication anything was stopped.
func TestDashboard_BackoffSection_RendersOnlyWhilePaused(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	d := newDashboard("/photos/2024", 10, 1000, newBatchTracker(10, 1000, 0, 0))

	// Running normally: nothing about backoff anywhere in the block.
	if got := strings.Join(d.batchLines(0), "\n"); strings.Contains(got, "THROTTLED") {
		t.Fatalf("batch block shows a pause before one has happened:\n%s", got)
	}

	d.onBackoff(activeBackoff())

	block := strings.Join(d.batchLines(0), "\n")
	for _, want := range []string{
		"Totals for batch:", // still inside the batch block, per the design
		"THROTTLED",
		"Quota exceeded for quota 'concurrent write request'", // why
		"rung 3/7", // where in the ladder
		"(3m)",     // how long this rung is
		"47s",      // live countdown
		"1/6",      // concurrency it will resume at, vs configured
	} {
		if !strings.Contains(block, want) {
			t.Errorf("paused batch block is missing %q:\n%s", want, block)
		}
	}
	// The boilerplate tail of the API message must be trimmed like every
	// other reason the dashboard shows.
	if strings.Contains(block, "of service") {
		t.Errorf("backoff reason was not trimmed:\n%s", block)
	}

	// Cleared: the section must vanish, not linger showing a stale countdown
	// while the pipeline is running again.
	d.onBackoff(uploader.BackoffStatus{Active: false})
	block = strings.Join(d.batchLines(0), "\n")
	if strings.Contains(block, "THROTTLED") || strings.Contains(block, "Backoff:") {
		t.Errorf("backoff section survived being cleared:\n%s", block)
	}
	if !strings.Contains(block, "Totals for batch:") {
		t.Errorf("clearing the backoff also removed the batch totals:\n%s", block)
	}
}

// TestDashboard_BackoffSection_ShownWithoutABatchBlock: the pause normally
// lives inside the batch totals, but a dashboard built without a
// batchTracker must still show it rather than silently dropping it.
func TestDashboard_BackoffSection_ShownWithoutABatchBlock(t *testing.T) {
	orig := color.NoColor
	color.NoColor = false // exercise the real repaint path
	defer func() { color.NoColor = orig }()

	d := newDashboard("/photos/2024", 1, 100, nil)
	out := captureStdout(t, func() { d.onBackoff(activeBackoff()) })
	if !strings.Contains(out, "THROTTLED") {
		t.Errorf("a dashboard with no batch block dropped the pause status entirely:\n%q", out)
	}
	if d.linesDrawn == 0 {
		t.Error("onBackoff left linesDrawn at 0 -- the block was not painted, so the next repaint's cursor math would be wrong")
	}
}

// TestDashboard_OnBackoff_RepaintsEveryTick: the countdown only advances on
// screen if each tick actually repaints. Dispatch is stopped during a
// pause, so no onBytes/onProgress events are arriving to trigger a repaint,
// and redrawThrottled's 200ms rate limiter must not swallow these.
func TestDashboard_OnBackoff_RepaintsEveryTick(t *testing.T) {
	orig := color.NoColor
	color.NoColor = false
	defer func() { color.NoColor = orig }()

	d := newDashboard("/photos/2024", 10, 1000, newBatchTracker(10, 1000, 0, 0))

	s := activeBackoff()
	d.onBackoff(s)
	first := d.lastRedrawAt

	// A second tick arriving well inside the throttle window must still paint.
	s.RemainingWait = 46 * time.Second
	d.onBackoff(s)
	second := d.lastRedrawAt

	if !second.After(first) {
		t.Error("a backoff tick did not repaint -- the countdown would sit frozen on screen for the whole pause")
	}
}

func TestShortDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{47 * time.Second, "47s"},
		{3 * time.Minute, "3m"},
		{90 * time.Second, "1m30s"},
		{15 * time.Minute, "15m"},
		{time.Hour, "1h"},
		{0, "0s"},
		{-5 * time.Second, "0s"},
		// Mid-tick values must not render as "46.999999s".
		{47*time.Second + 400*time.Millisecond, "47s"},
	}
	for _, c := range cases {
		if got := shortDuration(c.in); got != c.want {
			t.Errorf("shortDuration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestDashboard_TransferFigures_RateMatchesTheBytesShownBesideIt covers the
// reported oddity: "Transferred: 48.1MB / 48.1MB 100%" printed next to a
// rate that implied far fewer bytes over the same elapsed time.
//
// The percentage counted bytes actually sent (bytesDone + in-flight), while
// the rate counted only ledger-CONFIRMED bytes. At the tail of a folder --
// exactly where the user was looking -- files routinely have their transfer
// finished but their batchCreate not yet landed, so the two figures were
// computed from different numerators and could not both be right.
func TestDashboard_TransferFigures_RateMatchesTheBytesShownBesideIt(t *testing.T) {
	d := newDashboard("/photos/2024", 2, 1000, nil)
	d.start = time.Now().Add(-10 * time.Second)

	// 400 bytes confirmed, 600 sent but still awaiting batchCreate --
	// the state the tail of a folder is in.
	d.bytesDone = 400
	d.inFlight["/b.jpg"] = &fileTransfer{sent: 600, total: 600}

	doneOrInFlight := d.bytesDone + 600
	pct, rate, _ := d.transferFigures(doneOrInFlight)

	if pct != 100 {
		t.Errorf("pct = %v, want 100", pct)
	}
	// ~1000 bytes over ~10s, not ~400 over ~10s.
	if rate < 90 || rate > 110 {
		t.Errorf("rate = %.1f B/s, want ~100 (the 1000 bytes actually shown, over 10s) -- the rate is still being computed from a different figure than the one displayed next to it", rate)
	}
}

// TestDashboard_TransferFigures_ExcludesCircuitBreakerPauses: during a
// throttle pause nothing is sent by design. Counting that wall-clock time
// against the bytes made the rate collapse and stay wrong for the whole
// rest of the folder -- a 15-minute rung would report a near-zero rate even
// with a perfectly healthy pipeline either side of it.
func TestDashboard_TransferFigures_ExcludesCircuitBreakerPauses(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	d := newDashboard("/photos/2024", 1, 1000, nil)
	d.start = time.Now().Add(-10 * time.Second)
	d.bytesDone = 1000

	before := func() float64 { _, r, _ := d.transferFigures(1000); return r }()

	// A pause happens and ends, adding wall-clock time but no bytes.
	d.onBackoff(uploader.BackoffStatus{Active: true, RungWait: time.Second, RemainingWait: time.Second})
	d.mu.Lock()
	d.pauseStart = time.Now().Add(-30 * time.Second) // simulate a 30s pause
	d.start = d.start.Add(-30 * time.Second)         // ...which also advanced the clock
	d.mu.Unlock()
	d.onBackoff(uploader.BackoffStatus{Active: false})

	after := func() float64 { _, r, _ := d.transferFigures(1000); return r }()

	if after < before*0.8 {
		t.Errorf("rate fell from %.1f to %.1f B/s across a pause in which nothing was sent -- paused time must not count against the transfer rate", before, after)
	}
	if d.pausedTotal < 25*time.Second {
		t.Errorf("pausedTotal = %v, want ~30s recorded", d.pausedTotal)
	}
}

// TestDashboard_TransferFigures_NoDivideByZeroOnFirstPaint: the very first
// repaint happens microseconds after start.
func TestDashboard_TransferFigures_NoDivideByZeroOnFirstPaint(t *testing.T) {
	d := newDashboard("/photos/2024", 1, 1000, nil)
	_, rate, eta := d.transferFigures(0)
	if math.IsInf(rate, 0) || math.IsNaN(rate) {
		t.Errorf("rate = %v on the first paint, want a finite number", rate)
	}
	if eta != "-" {
		t.Errorf("eta = %q with nothing transferred yet, want %q", eta, "-")
	}
}

// TestBlockLines_BoundedHeight_SoTheRepaintMathHolds guards the live
// block's one hard constraint: it has to fit on screen.
//
// redraw() repaints by moving the cursor up linesDrawn lines and
// overwriting in place. The moment the block is taller than the terminal,
// the terminal scrolls it, "up N lines" no longer reaches the top of it,
// and each repaint smears a partial copy down the screen instead of
// replacing the last one -- reported as pages of duplicated, half-
// overwritten blocks: "something is completely off with the output now".
//
// A file leaves inFlight only when onProgress fires, which is after
// mediaItems.batchCreate commits, so up to batchFlushCount (50) files can
// be parked there at 100% waiting on a single flush. That is the state
// this reproduces.
func TestBlockLines_BoundedHeight_SoTheRepaintMathHolds(t *testing.T) {
	d := &dashboard{
		start:      time.Now(),
		folder:     "(entire library, smallest-first)",
		inFlight:   map[string]*fileTransfer{},
		filesTotal: 15583,
		bytesTotal: 204 << 30,
		batch:      newBatchTracker(15583, 204<<30, 0, 0),
		active:     true,
	}
	// 50 files done sending, parked until the batch commits.
	for i := 0; i < 50; i++ {
		d.inFlight[fmt.Sprintf("C:/Photos/IMG-2021%04d.jpg", i)] = &fileTransfer{sent: 315000, total: 315000}
	}
	// Two genuinely mid-transfer, which is what a person is watching for.
	d.inFlight["C:/Photos/active-one.jpg"] = &fileTransfer{sent: 32000, total: 319000}
	d.inFlight["C:/Photos/active-two.jpg"] = &fileTransfer{sent: 96000, total: 400000}

	lines := d.blockLines()

	// The fixed part is ~8 lines (batch block, folder, transferred, files,
	// the "Uploading:" header, the overflow note); the cap is what keeps
	// the total inside even a small terminal.
	if len(lines) > maxInFlightRows+10 {
		t.Errorf("block is %d lines with 52 files in flight -- it must stay bounded or the cursor-up repaint breaks:\n%s",
			len(lines), strings.Join(lines, "\n"))
	}

	// Both actively-transferring files must survive the cap: hiding those
	// in favour of 12 identical 100% rows would defeat the point of the
	// list entirely.
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"active-one.jpg", "active-two.jpg"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s is mid-transfer and was hidden by the cap; block:\n%s", want, joined)
		}
	}
	// And the hidden ones have to be accounted for, not silently dropped.
	if !strings.Contains(joined, "and 40 more") {
		t.Errorf("expected the 40 capped-off rows to be summarised; block:\n%s", joined)
	}
}

// TestBlockLines_UnderTheCap_ListsEveryFile is the control: the cap must
// only engage when it has to, or a normal run (concurrency-many files in
// flight) would start hiding rows for no reason.
func TestBlockLines_UnderTheCap_ListsEveryFile(t *testing.T) {
	d := &dashboard{
		start:    time.Now(),
		inFlight: map[string]*fileTransfer{},
		active:   true,
	}
	for i := 0; i < 6; i++ {
		d.inFlight[fmt.Sprintf("C:/Photos/f%d.jpg", i)] = &fileTransfer{sent: int64(i) * 1000, total: 10000}
	}

	joined := strings.Join(d.blockLines(), "\n")
	for i := 0; i < 6; i++ {
		if want := fmt.Sprintf("f%d.jpg", i); !strings.Contains(joined, want) {
			t.Errorf("%s missing from an under-cap block:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "more, sent and waiting") {
		t.Errorf("the overflow note appeared with only 6 files in flight:\n%s", joined)
	}
}
