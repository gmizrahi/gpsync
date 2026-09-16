package watchctl

import (
	"github.com/gmizrahi/gpsync/internal/dashboard"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/statedb"
	"github.com/gmizrahi/gpsync/internal/uploader"
)

func openTestDB(t *testing.T) *statedb.DB {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GPSYNC_STATE_DIR", dir)
	statedb.StateDir = dir
	statedb.StateDBPath = filepath.Join(dir, "state.sqlite")
	db, err := statedb.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestNew_ConfigRoundTrips(t *testing.T) {
	db := openTestDB(t)
	cfg := config.Defaults()
	cfg.Concurrency = 3
	wc := New(db, cfg)

	if got := wc.Config().Concurrency; got != 3 {
		t.Errorf("Config().Concurrency = %d, want 3", got)
	}
	if wc.IsRunning() {
		t.Error("a freshly-constructed controller must not report running")
	}

	updated := cfg
	updated.Concurrency = 9
	wc.SetConfig(updated)
	if got := wc.Config().Concurrency; got != 9 {
		t.Errorf("Config().Concurrency after SetConfig = %d, want 9", got)
	}
}

func TestSetSourceFoldersOverride_SurvivesSetConfig(t *testing.T) {
	db := openTestDB(t)
	cfg := config.Defaults()
	wc := New(db, cfg)
	wc.SetSourceFoldersOverride([]string{"/lib/photos"})

	if got := wc.Config().SourceFolders; len(got) != 1 || got[0] != "/lib/photos" {
		t.Fatalf("Config().SourceFolders after SetSourceFoldersOverride = %v, want [/lib/photos]", got)
	}

	// The exact real bug: a Settings save loads a FRESH config from disk
	// (empty SourceFolders for a CLI-only invocation) and calls SetConfig
	// with it -- this must not be able to silently clobber the override.
	fresh := config.Defaults()
	fresh.Theme = config.ThemeDark
	fresh.SourceFolders = nil
	wc.SetConfig(fresh)

	got := wc.Config()
	if len(got.SourceFolders) != 1 || got.SourceFolders[0] != "/lib/photos" {
		t.Errorf("SourceFolders after SetConfig(fresh) = %v, want the override [/lib/photos] to survive", got.SourceFolders)
	}
	if got.Theme != config.ThemeDark {
		t.Errorf("Theme = %q, want %q -- every OTHER field must still update normally", got.Theme, config.ThemeDark)
	}
}

func TestSetConfig_NoOverride_UpdatesSourceFoldersNormally(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults()) // no override set -- gpsync-tray's own case

	updated := config.Defaults()
	updated.SourceFolders = []string{"/new/folder"}
	wc.SetConfig(updated)

	if got := wc.Config().SourceFolders; len(got) != 1 || got[0] != "/new/folder" {
		t.Errorf("SourceFolders = %v, want [/new/folder] -- SetConfig must behave exactly as before when no override is set", got)
	}
}

func TestStartStop_NoSourceFolders_ExitsCleanlyAndIsNotRunningAfterStop(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults()) // no SourceFolders set

	wc.Start()
	// run() logs "no source folders configured" and returns almost
	// immediately -- Stop() must still be safe to call (a no-op on an
	// already-finished run, not a hang) and IsRunning() must end up
	// false either way.
	wc.Stop()
	if wc.IsRunning() {
		t.Error("IsRunning() = true after Stop(), want false")
	}
}

func TestStart_Idempotent_SecondCallIsNoOp(t *testing.T) {
	db := openTestDB(t)
	cfg := config.Defaults()
	cfg.SourceFolders = []string{t.TempDir()}
	wc := New(db, cfg)

	wc.Start()
	defer wc.Stop()
	// A second Start() while already running must not replace the
	// in-flight run's channels out from under it (the exact WaitGroup-
	// style race this package's own doc comment describes fixing).
	wc.Start()
	if !wc.IsRunning() {
		t.Error("IsRunning() = false after Start(), want true")
	}
}

func TestStop_NotRunning_IsANoOp(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())
	wc.Stop() // must not panic or block
	if wc.IsRunning() {
		t.Error("IsRunning() = true, want false")
	}
}

func TestRescanNowRequestRetryNowCancelFile_NoOpWhenNotRunning(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())

	wc.RescanNow()       // must not panic
	wc.RequestRetryNow() // must not panic
	if wc.CancelFile("/some/path") {
		t.Error("CancelFile() = true when nothing is running, want false")
	}
}

func TestOnBytes_TracksInFlightFiles(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())

	wc.onBytes(uploader.ByteProgressEvent{Path: "/lib/a.jpg", Sent: 50, Total: 100})
	wc.onBytes(uploader.ByteProgressEvent{Path: "/lib/b.jpg", Sent: 10, Total: 20})

	inFlight := wc.InFlightFiles()
	if len(inFlight) != 2 {
		t.Fatalf("len(InFlightFiles()) = %d, want 2", len(inFlight))
	}
	if inFlight["/lib/a.jpg"].Sent != 50 || inFlight["/lib/a.jpg"].Total != 100 {
		t.Errorf("InFlightFiles()[/lib/a.jpg] = %+v, want Sent:50 Total:100", inFlight["/lib/a.jpg"])
	}

	// A later chunk for the same path overwrites, doesn't accumulate.
	wc.onBytes(uploader.ByteProgressEvent{Path: "/lib/a.jpg", Sent: 100, Total: 100})
	if got := wc.InFlightFiles()["/lib/a.jpg"].Sent; got != 100 {
		t.Errorf("Sent after second onBytes = %d, want 100 (overwritten, not summed)", got)
	}
}

func TestOnProgress_CompletionClearsInFlightAndAppendsToRecent(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())

	wc.onBytes(uploader.ByteProgressEvent{Path: "/lib/a.jpg", Sent: 50, Total: 100})
	wc.onProgress(uploader.ProgressEvent{LastFile: "/lib/a.jpg", LastOK: true, LastSize: 100})

	if _, stillInFlight := wc.InFlightFiles()["/lib/a.jpg"]; stillInFlight {
		t.Error("a completed file must be removed from InFlightFiles")
	}
	recent := wc.RecentEvents()
	if len(recent) != 1 || recent[0].LastFile != "/lib/a.jpg" {
		t.Fatalf("RecentEvents() = %+v, want one entry for /lib/a.jpg", recent)
	}

	if uploaded, uploading, _ := wc.FileProgress(); uploaded != 1 || uploading != 0 {
		t.Errorf("FileProgress() uploaded=%d uploading=%d, want 1 and 0", uploaded, uploading)
	}
	bytesDone, _, _ := wc.RunProgress()
	if bytesDone != 100 {
		t.Errorf("RunProgress() bytesDone = %d, want 100", bytesDone)
	}
}

func TestOnProgress_DeferredEvent_ClearsInFlightButNotLoggedToRecent(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())

	wc.onBytes(uploader.ByteProgressEvent{Path: "/lib/a.jpg", Sent: 10, Total: 100})
	wc.onProgress(uploader.ProgressEvent{LastFile: "/lib/a.jpg", LastDeferred: true})

	if _, stillInFlight := wc.InFlightFiles()["/lib/a.jpg"]; stillInFlight {
		t.Error("a deferred (throttled) file must still be removed from InFlightFiles")
	}
	if len(wc.RecentEvents()) != 0 {
		t.Error("a deferred file is not a real completion -- it must not appear in RecentEvents")
	}
	if uploaded, uploading, _ := wc.FileProgress(); uploaded != 0 || uploading != 0 {
		t.Errorf("FileProgress() uploaded=%d uploading=%d, want 0 and 0 -- a deferred file hasn't finished and isn't sending", uploaded, uploading)
	}
}

// Uploaded, uploading and total are reported separately. A combined
// "uploaded + in flight" figure went DOWN whenever a throttle failed the
// files being sent; uploaded alone must never decrease within a cycle.
func TestFileProgress_UploadedNeverDecreasesWhenInFlightFilesFail(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())
	wc.setPendingTotals(7, 0)

	check := func(step string, wantUploaded, wantUploading, wantTotal int) {
		t.Helper()
		if u, f, tot := wc.FileProgress(); u != wantUploaded || f != wantUploading || tot != wantTotal {
			t.Errorf("%s: FileProgress() = (%d uploaded, %d uploading, %d total), want (%d, %d, %d)",
				step, u, f, tot, wantUploaded, wantUploading, wantTotal)
		}
	}
	check("before anything starts", 0, 0, 7)

	for _, p := range []string{"/lib/a.jpg", "/lib/b.jpg", "/lib/c.jpg", "/lib/d.jpg", "/lib/e.jpg"} {
		wc.onBytes(uploader.ByteProgressEvent{Path: p, Sent: 10, Total: 100})
	}
	check("five files sending", 0, 5, 7)

	// Four succeed, one fails: the failure counts as neither, and the
	// total does not shrink.
	for _, p := range []string{"/lib/a.jpg", "/lib/b.jpg", "/lib/c.jpg", "/lib/d.jpg"} {
		wc.onProgress(uploader.ProgressEvent{LastFile: p, LastOK: true, LastSize: 100})
	}
	wc.onProgress(uploader.ProgressEvent{LastFile: "/lib/e.jpg", LastOK: false})
	wc.setPendingTotals(2, 0)
	check("four uploaded, one failed", 4, 0, 7)

	wc.onBytes(uploader.ByteProgressEvent{Path: "/lib/f.jpg", Sent: 10, Total: 100})
	wc.onBytes(uploader.ByteProgressEvent{Path: "/lib/g.jpg", Sent: 10, Total: 100})
	check("two more sending", 4, 2, 7)

	// A throttle defers both back into the queue: uploading drops to 0,
	// uploaded stays at 4.
	wc.onProgress(uploader.ProgressEvent{LastFile: "/lib/f.jpg", LastDeferred: true})
	wc.onProgress(uploader.ProgressEvent{LastFile: "/lib/g.jpg", LastDeferred: true})
	check("throttled", 4, 0, 7)
}

// TestFileProgress_CompletingAnInFlightFileDoesNotDoubleCount guards why
// FileProgress reads both counts under one lock: a file moves OUT of
// inFlight and INTO filesSucceeded in one step, so it is never counted as
// both.
func TestFileProgress_CompletingAnInFlightFileDoesNotDoubleCount(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())

	wc.onBytes(uploader.ByteProgressEvent{Path: "/lib/a.jpg", Sent: 50, Total: 100})
	if uploaded, uploading, _ := wc.FileProgress(); uploaded != 0 || uploading != 1 {
		t.Fatalf("while in flight: uploaded=%d uploading=%d, want 0 and 1", uploaded, uploading)
	}
	wc.onProgress(uploader.ProgressEvent{LastFile: "/lib/a.jpg", LastOK: true, LastSize: 100})
	if uploaded, uploading, _ := wc.FileProgress(); uploaded != 1 || uploading != 0 {
		t.Errorf("after it completes: uploaded=%d uploading=%d, want 1 and 0 -- never counted as both", uploaded, uploading)
	}
}

func TestRecentEvents_CappedAtMaxAndMostRecentFirst(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())

	for i := 0; i < maxRecentEvents+5; i++ {
		wc.onProgress(uploader.ProgressEvent{LastFile: string(rune('a' + i)), LastOK: true})
	}
	recent := wc.RecentEvents()
	if len(recent) != maxRecentEvents {
		t.Fatalf("len(RecentEvents()) = %d, want %d (capped)", len(recent), maxRecentEvents)
	}
	// The LAST file appended must be FIRST in the returned slice.
	last := string(rune('a' + maxRecentEvents + 4))
	if recent[0].LastFile != last {
		t.Errorf("RecentEvents()[0] = %q, want %q (most recent first)", recent[0].LastFile, last)
	}
}

func TestSetBackoff_ActiveVsInactive(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())

	if wc.CurrentBackoff() != nil {
		t.Fatal("CurrentBackoff() should start nil")
	}
	wc.setBackoff(uploader.BackoffStatus{Active: true, Reason: "throttled", Rung: 2})
	if b := wc.CurrentBackoff(); b == nil || b.Reason != "throttled" {
		t.Errorf("CurrentBackoff() = %+v, want an active status with Reason=throttled", b)
	}
	wc.setBackoff(uploader.BackoffStatus{Active: false})
	if wc.CurrentBackoff() != nil {
		t.Error("CurrentBackoff() should be nil again once Active=false is reported")
	}
}

// TestLogAndRecordCrash_PersistsToLedger covers the testable half of the
// watch-loop panic recovery -- the os.Exit(1) that follows a REAL panic
// in Start's own deferred recover is an unavoidably untestable side
// effect in-process (matching cmd/gpsync-tray/crashrecovery.go's own
// recoverAndLog, which has the identical constraint and no test either).
// This proves the logging/recording half -- which is the actual payoff,
// a permanent record in crash_log -- works correctly on its own.
func TestLogAndRecordCrash_PersistsToLedger(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())

	wc.logAndRecordCrash("watch loop", "index out of range [5] with length 3")

	rec, ok, err := db.LastCrash()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("LastCrash() ok = false, want a recorded crash")
	}
	if rec.Context != "watch loop" {
		t.Errorf("Context = %q, want %q", rec.Context, "watch loop")
	}
	if rec.Message != "index out of range [5] with length 3" {
		t.Errorf("Message = %q, want the panic value's string form", rec.Message)
	}
}

func TestBeginRunAndSetPendingTotals(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())

	wc.beginRun(1000)
	bytesDone, bytesTotal, start := wc.RunProgress()
	if bytesDone != 0 || bytesTotal != 1000 {
		t.Errorf("after beginRun: bytesDone=%d bytesTotal=%d, want 0/1000", bytesDone, bytesTotal)
	}
	if start.IsZero() || time.Since(start) > time.Second {
		t.Errorf("RunProgress() start = %v, want a recent non-zero timestamp", start)
	}

	wc.onProgress(uploader.ProgressEvent{LastFile: "/a.jpg", LastOK: true, LastSize: 100})
	wc.setPendingTotals(5, 500)
	_, _, filesTotal := wc.FileProgress()
	if filesTotal != 1+5 {
		t.Errorf("FileProgress() total = %d, want 6 (1 done + 5 still pending)", filesTotal)
	}
	_, bytesTotal, _ = wc.RunProgress()
	if bytesTotal != 100+500 {
		t.Errorf("RunProgress() bytesTotal = %d, want 600 (100 done + 500 still pending)", bytesTotal)
	}
}

// Throttle backoff is excluded from the cycle's transfer time, as in the
// CLI. After five backoff rungs (5m to 60m) the tray showed 38 KB/s and a
// 225h ETA for a folder that uploads at several MB/s.
func TestRunPausedFor_CountsBackoffAndResetsPerCycle(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())
	wc.beginRun(1000)

	wc.setBackoff(uploader.BackoffStatus{Active: true})
	time.Sleep(30 * time.Millisecond)
	if p := wc.RunPausedFor(); p < 25*time.Millisecond {
		t.Errorf("during a pause RunPausedFor() = %v, want the ongoing pause counted", p)
	}
	wc.setBackoff(uploader.BackoffStatus{Active: false})
	ended := wc.RunPausedFor()
	time.Sleep(20 * time.Millisecond)
	if p := wc.RunPausedFor(); p != ended {
		t.Errorf("RunPausedFor() grew from %v to %v after the pause ended", ended, p)
	}

	wc.beginRun(1000)
	if p := wc.RunPausedFor(); p != 0 {
		t.Errorf("after beginRun RunPausedFor() = %v, want 0", p)
	}

	// A cycle that begins mid-pause counts the pause from its own start.
	wc.setBackoff(uploader.BackoffStatus{Active: true})
	time.Sleep(200 * time.Millisecond)
	wc.beginRun(1000)
	time.Sleep(20 * time.Millisecond)
	if p := wc.RunPausedFor(); p < 15*time.Millisecond || p > 150*time.Millisecond {
		t.Errorf("cycle begun mid-pause: RunPausedFor() = %v, want only the ~20ms since beginRun, not the 200ms before it", p)
	}
}

// A finished or aborted cycle must not keep showing as a running one: the
// Run line once read "103 total, 1h 27m elapsed" with the clock still
// ticking long after the cycle gave up on throttling.
func TestEndRun_ClearsTheCycleCounters(t *testing.T) {
	db := openTestDB(t)
	wc := New(db, config.Defaults())
	wc.beginRun(1000)
	wc.setPendingTotals(103, 11_600)
	wc.onBytes(uploader.ByteProgressEvent{Path: "/lib/a.mp4", Sent: 10, Total: 100})
	wc.onProgress(uploader.ProgressEvent{LastFile: "/lib/a.mp4", LastOK: true, LastSize: 100})

	wc.endRun()
	if u, f, tot := wc.FileProgress(); u != 0 || f != 0 || tot != 0 {
		t.Errorf("FileProgress() after endRun = (%d, %d, %d), want all zero", u, f, tot)
	}
	if done, total, start := wc.RunProgress(); done != 0 || total != 0 || !start.IsZero() {
		t.Errorf("RunProgress() after endRun = (%d, %d, %v), want zeros and no start time", done, total, start)
	}
}

// WatchController is what the web dashboard drives. Asserting that here, in
// the test binary, keeps the compile-time check without making the
// production package depend on the UI it happens to serve.
var _ dashboard.Controller = (*WatchController)(nil)
