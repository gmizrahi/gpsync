package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/auth"
	"github.com/gmizrahi/gpsync/internal/config"
	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/hashing"
	"github.com/gmizrahi/gpsync/internal/quota"
	"github.com/gmizrahi/gpsync/internal/uploader"
)

// withFakeOAuthCredentials points auth.ClientSecretPath/TokenPath at a
// syntactically-valid but fake client_secret.json/token.json in dir, so
// uploader.New's auth.GetHTTPClient succeeds without a real OAuth consent
// flow or network round trip -- oauth2.ReuseTokenSource only actually
// refreshes when the token is used AND expired, and this token's expiry is
// far in the future, so New() returns a usable (if fake-authenticated)
// *http.Client purely from local files. Needed to exercise
// RunFolderCycle's upload-phase loop logic (as opposed to just proving
// uploader.New fails without credentials, like the other tests in this
// file) -- restores the real paths via t.Cleanup.
func withFakeOAuthCredentials(t *testing.T, dir string) {
	t.Helper()
	origSecret, origToken := auth.ClientSecretPath, auth.TokenPath
	auth.ClientSecretPath = filepath.Join(dir, "client_secret.json")
	auth.TokenPath = filepath.Join(dir, "token.json")
	t.Cleanup(func() { auth.ClientSecretPath, auth.TokenPath = origSecret, origToken })

	if err := os.WriteFile(auth.ClientSecretPath, []byte(`{"client_id":"fake","client_secret":"fake"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth.TokenPath, []byte(`{"access_token":"fake","token_type":"Bearer","refresh_token":"fake","expiry":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRunFolderCycle_AlreadySynced_SkipsUploadPhaseEntirely proves the
// no-op short-circuit: a folder with nothing pending and nothing new to
// scan must never reach uploader.New (which would need a real OAuth
// client this test doesn't have) -- this is the common case gpsync-tray's
// backlog/heartbeat will hit constantly once a library is caught up.
func TestRunFolderCycle_AlreadySynced_SkipsUploadPhaseEntirely(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(p, []byte("already-synced-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := hashing.SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	mtime := float64(info.ModTime().UnixNano()) / 1e9
	mustNoErr(t, db.UpsertFileSeen(p, sum, mtime, info.Size()))
	mustNoErr(t, db.EnsurePending(sum, info.Size(), "image/jpeg", p, nil))
	mustNoErr(t, db.MarkUploaded(sum, "media-1", ""))

	conc := quota.NewAdaptiveConcurrency(1)
	breaker := uploader.NewCircuitBreakerState()

	result, err := RunFolderCycle(context.Background(), db, config.Defaults(), dir, conc, breaker, nil, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunFolderCycle = %v, want nil -- an already-synced folder must never reach the upload phase", err)
	}
	if result.Scan.NewPending != 0 {
		t.Errorf("Scan.NewPending = %d, want 0", result.Scan.NewPending)
	}
	if result.Upload != (uploader.Stats{}) {
		t.Errorf("Upload = %+v, want zero value -- upload phase must not have run at all", result.Upload)
	}
}

// TestRunFolderCycle_PendingWork_StillAttemptsUpload guards the safety
// check symmetric to cmd/gpsync's syncOneFolder: a folder can have real
// upload work outstanding from an earlier interrupted run even when THIS
// scan finds nothing new to hash, and that must never be silently
// skipped. Since this test has no real OAuth client, the upload phase is
// expected to fail -- what matters is that it was actually ATTEMPTED, not
// silently short-circuited like the already-synced case above.
func TestRunFolderCycle_PendingWork_StillAttemptsUpload(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(p, []byte("still-pending-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := hashing.SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	mtime := float64(info.ModTime().UnixNano()) / 1e9
	mustNoErr(t, db.UpsertFileSeen(p, sum, mtime, info.Size()))
	mustNoErr(t, db.EnsurePending(sum, info.Size(), "image/jpeg", p, nil))
	// Deliberately NOT marked uploaded.

	conc := quota.NewAdaptiveConcurrency(1)
	breaker := uploader.NewCircuitBreakerState()

	_, err = RunFolderCycle(context.Background(), db, config.Defaults(), dir, conc, breaker, nil, nil, nil, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("RunFolderCycle = nil error, want the upload phase to have actually been attempted (and fail, with no OAuth client configured in this test)")
	}
}

// TestRunFolderCycle_FailedRetryableWithNothingElsePending_StillAttemptsUpload
// covers a bug found in production: a folder whose ONLY
// unsynced files are already failed_retryable (a prior throttle-schedule
// abort, say), with nothing genuinely 'pending' and nothing new for the
// scan to find, used to be silently treated as fully caught up FOREVER.
// RequeueRetryable (which moves failed_retryable rows back to pending) has
// always lived inside Uploader.Run() itself, but the loop above only ever
// calls Run() when db.ListPendingUnder (status='pending' only) already
// finds something -- failed_retryable isn't 'pending' until something
// requeues it, so nothing would ever call Run() again, so it would never
// get requeued either. The symptom: `gpsync sync` reporting a folder
// "already synced" that actually had failed_retryable files sitting
// untouched, and gpsync-tray's dashboard going idle for over an hour with a
// stale run_progress timestamp after a mass throttle abort. This seeds a
// file that's cache-hit-scanned (so the scan finds nothing new) AND
// already failed_retryable (so nothing is 'pending' either) -- exactly the
// stuck scenario -- and proves the upload phase still gets attempted.
func TestRunFolderCycle_FailedRetryableWithNothingElsePending_StillAttemptsUpload(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(p, []byte("stuck-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := hashing.SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	mtime := float64(info.ModTime().UnixNano()) / 1e9
	mustNoErr(t, db.UpsertFileSeen(p, sum, mtime, info.Size()))
	mustNoErr(t, db.EnsurePending(sum, info.Size(), "image/jpeg", p, nil))
	mustNoErr(t, db.MarkFailed(sum, false, "THROTTLE_BACKOFF_EXHAUSTED", "gave up after 34m40s of throttle backoff"))

	row, err := db.GetUpload(sum)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status != "failed_retryable" {
		t.Fatalf("precondition failed: status = %q, want failed_retryable", row.Status)
	}

	conc := quota.NewAdaptiveConcurrency(1)
	breaker := uploader.NewCircuitBreakerState()

	_, err = RunFolderCycle(context.Background(), db, config.Defaults(), dir, conc, breaker, nil, nil, nil, nil, nil, nil, nil)
	if err == nil {
		t.Fatal("RunFolderCycle = nil error, want the upload phase to have actually been attempted (and fail, with no OAuth client configured in this test) -- a failed_retryable-only backlog must not be silently treated as fully caught up")
	}
}

// TestRunFolderCycle_UploadStartsWithoutWaitingForScanToFinish is the
// actual concurrency fix: "why do you wait before your scan gets to the
// folder; start the upload right away, the scan and queueing of new
// discovered files should be done in a parallel thread." Proves it with
// real timing, not just call ordering: the folder has one file already
// pending from an earlier run (so the upload loop has real work on its
// very first pass, before the scan goroutine has reported anything) PLUS a
// large (2GB) decoy file the scan has to actually hash. uploader.New fails
// FAST in this test environment (a plain missing-credentials-file error,
// no network round-trip -- see TestRunFolderCycle_PendingWork_
// StillAttemptsUpload), so RunFolderCycle returning well under the decoy
// file's own hash time is only possible if the upload loop's first
// uploader.New call fired before -- not after -- the scan finished hashing
// it.
//
// Sized with real margin after two smaller attempts (200MB, then 600MB)
// turned out FLAKY against the OLD sequential RunFolderCycle -- both
// occasionally finished hashing fast enough (page cache, an unloaded
// machine) to sneak in under a few-hundred-ms threshold even without the
// fix, which would have made this a test that mostly, not reliably,
// caught a regression. 2GB (~1.5-2s to hash, extrapolated from a ~470ms/
// 600MB benchmark) against a 500ms assertion leaves real headroom on both
// sides: the concurrent (fixed) path never touches the decoy before
// returning, so it stays in the low single-digit ms regardless of size.
func TestRunFolderCycle_UploadStartsWithoutWaitingForScanToFinish(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()

	decoy := filepath.Join(dir, "decoy.dat")
	if err := os.WriteFile(decoy, make([]byte, 2*1024*1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}

	// Already pending from an earlier run -- no file on disk needed for
	// this row itself, same established pattern as
	// TestRunFolderCycle_PendingWork_StillAttemptsUpload.
	mustNoErr(t, db.EnsurePending("hash-already-pending", 123, "image/jpeg", filepath.Join(dir, "already-pending.jpg"), nil))

	conc := quota.NewAdaptiveConcurrency(1)
	breaker := uploader.NewCircuitBreakerState()

	start := time.Now()
	_, err := RunFolderCycle(context.Background(), db, config.Defaults(), dir, conc, breaker, nil, nil, nil, nil, nil, nil, nil)
	elapsed := time.Since(start)

	// RunFolderCycle deliberately does NOT wait for its background scan
	// goroutine on an early return (see its own doc comment) -- which is
	// exactly what this test is proving. But that goroutine is still
	// running against this test's db/decoy file after the assertions
	// below, so wait for it to actually finish (evidenced by the decoy
	// showing up as pending too) before this test function returns and
	// t.Cleanup tears down the temp dir and closes db out from under it.
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			pending, perr := db.ListPendingUnder([]string{dir})
			if perr == nil && len(pending) >= 2 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	if err == nil {
		t.Fatal("RunFolderCycle = nil error, want the upload phase to have actually been attempted (and fail, with no OAuth client configured in this test)")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("RunFolderCycle took %v, want well under the decoy file's own hash time -- the upload phase must not wait for the scan to finish", elapsed)
	}
}

// TestRunFolderCycle_AllPendingExcludedByMediaTypeFilter_StopsInsteadOfSpinning
// covers the bug where a folder whose ENTIRE pending set is
// excluded by cfg.MediaTypeFilter (e.g. "photos only" while everything
// pending is a video) used to spin this loop forever -- Run() returns
// cleanly with SkippedMediaType>0 and no error, so `pending` at the top of
// the loop was identical lap after lap, with no ctx check ever reached
// (that only happens in the "wait for the wake channel" branch, never hit
// while pending stays non-empty). Verified by execution: 100% CPU,
// unresponsive even to context cancellation. Uses fake OAuth credentials
// (see withFakeOAuthCredentials) specifically so the upload phase's own
// filtering logic actually runs, unlike this file's other tests which stop
// at uploader.New's credential check -- no real HTTP request is made here
// regardless, since every row is excluded before any network call.
func TestRunFolderCycle_AllPendingExcludedByMediaTypeFilter_StopsInsteadOfSpinning(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	withFakeOAuthCredentials(t, t.TempDir()) // separate dir -- not itself scanned as part of the folder under test

	p := filepath.Join(dir, "clip.mp4")
	if err := os.WriteFile(p, []byte("video-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := hashing.SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	mtime := float64(info.ModTime().UnixNano()) / 1e9
	mustNoErr(t, db.UpsertFileSeen(p, sum, mtime, info.Size()))
	mustNoErr(t, db.EnsurePending(sum, info.Size(), "video/mp4", p, nil))

	cfg := config.Defaults()
	cfg.MediaTypeFilter = extensions.KindPhoto // the only pending file is a video

	conc := quota.NewAdaptiveConcurrency(1)
	breaker := uploader.NewCircuitBreakerState()

	done := make(chan error, 1)
	go func() {
		_, rerr := RunFolderCycle(context.Background(), db, cfg, dir, conc, breaker, nil, nil, nil, nil, nil, nil, nil)
		done <- rerr
	}()

	select {
	case rerr := <-done:
		var abortErr *uploader.AbortError
		if !errors.As(rerr, &abortErr) {
			t.Fatalf("RunFolderCycle error = %v, want an *uploader.AbortError", rerr)
		}
		if abortErr.Reason != uploader.AbortNoEligibleFiles {
			t.Errorf("AbortError.Reason = %q, want %q", abortErr.Reason, uploader.AbortNoEligibleFiles)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunFolderCycle did not return within 5s -- it is spinning forever, exactly the bug this test guards against")
	}
}

// TestRunFolderCycle_PhotosFirstWithUnknownKindExtension_StopsInsteadOfSpinning
// is the SECOND real trigger for the same bug, specific to the multi-pass
// strategies: SyncStrategyPhotosFirst splits a lap into two passes (one
// MediaTypeFilter=KindPhoto, one KindVideo -- see uploadPasses). A `.webm`
// file (deliberately not in extensions.supportedExts, so
// extensions.KindOf returns KindUnknown -- attempted, per this package's
// own "not exhaustive" design, so extensions.Classify never marks it
// Unsupported/FailedPermanent either) is excluded by BOTH passes at once,
// so this exercises lapProgress accumulating ACROSS every pass in a lap,
// not just a single Run() call's own Stats -- a single-pass test (like the
// one above) can't tell the difference between "this pass made no
// progress" and "no pass in the whole lap did."
func TestRunFolderCycle_PhotosFirstWithUnknownKindExtension_StopsInsteadOfSpinning(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()
	withFakeOAuthCredentials(t, t.TempDir())

	p := filepath.Join(dir, "clip.webm")
	if err := os.WriteFile(p, []byte("webm-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := hashing.SHA256File(p)
	if err != nil {
		t.Fatal(err)
	}
	if extensions.KindOf(p) != extensions.KindUnknown {
		t.Fatalf("precondition failed: .webm must classify as KindUnknown for this test to exercise what it claims")
	}
	mtime := float64(info.ModTime().UnixNano()) / 1e9
	mustNoErr(t, db.UpsertFileSeen(p, sum, mtime, info.Size()))
	mustNoErr(t, db.EnsurePending(sum, info.Size(), "video/webm", p, nil))

	cfg := config.Defaults()
	cfg.SyncStrategy = config.SyncStrategyPhotosFirst

	conc := quota.NewAdaptiveConcurrency(1)
	breaker := uploader.NewCircuitBreakerState()

	done := make(chan error, 1)
	go func() {
		_, rerr := RunFolderCycle(context.Background(), db, cfg, dir, conc, breaker, nil, nil, nil, nil, nil, nil, nil)
		done <- rerr
	}()

	select {
	case rerr := <-done:
		var abortErr *uploader.AbortError
		if !errors.As(rerr, &abortErr) {
			t.Fatalf("RunFolderCycle error = %v, want an *uploader.AbortError", rerr)
		}
		if abortErr.Reason != uploader.AbortNoEligibleFiles {
			t.Errorf("AbortError.Reason = %q, want %q", abortErr.Reason, uploader.AbortNoEligibleFiles)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunFolderCycle did not return within 5s -- it is spinning forever, exactly the bug this test guards against")
	}
}

// TestUploadPasses_FolderByFolder_SinglePassUnchanged is the default: no
// SyncStrategy set (or explicitly folder_by_folder) must produce exactly
// one pass with cfg untouched.
func TestUploadPasses_FolderByFolder_SinglePassUnchanged(t *testing.T) {
	cfg := config.Defaults()
	cfg.MediaTypeFilter = extensions.KindPhoto // sentinel: must survive unchanged

	passes := uploadPasses(cfg)
	if len(passes) != 1 {
		t.Fatalf("uploadPasses = %d passes, want 1", len(passes))
	}
	if !reflect.DeepEqual(passes[0], cfg) {
		t.Errorf("passes[0] = %+v, want cfg unchanged", passes[0])
	}
}

// TestUploadPasses_SmallestFirst_ForcesSortOnASinglePass proves the
// "smallest to largest" strategy is one pass with SortSmallestFirst
// forced on, not a new sorting mechanism.
func TestUploadPasses_SmallestFirst_ForcesSortOnASinglePass(t *testing.T) {
	cfg := config.Defaults()
	cfg.SyncStrategy = config.SyncStrategySmallestFirst
	cfg.SortSmallestFirst = false

	passes := uploadPasses(cfg)
	if len(passes) != 1 {
		t.Fatalf("uploadPasses = %d passes, want 1", len(passes))
	}
	if !passes[0].SortSmallestFirst {
		t.Error("passes[0].SortSmallestFirst = false, want true")
	}
}

// TestUploadPasses_PhotosFirst_TwoPasses proves "photos before videos"
// produces exactly two passes, photos then videos, when MediaTypeFilter
// isn't already narrowed.
func TestUploadPasses_PhotosFirst_TwoPasses(t *testing.T) {
	cfg := config.Defaults()
	cfg.SyncStrategy = config.SyncStrategyPhotosFirst

	passes := uploadPasses(cfg)
	if len(passes) != 2 {
		t.Fatalf("uploadPasses = %d passes, want 2", len(passes))
	}
	if passes[0].MediaTypeFilter != extensions.KindPhoto {
		t.Errorf("passes[0].MediaTypeFilter = %v, want KindPhoto", passes[0].MediaTypeFilter)
	}
	if passes[1].MediaTypeFilter != extensions.KindVideo {
		t.Errorf("passes[1].MediaTypeFilter = %v, want KindVideo", passes[1].MediaTypeFilter)
	}
}

// TestUploadPasses_PhotosFirst_AlreadyNarrowed_SinglePass proves that when
// the user has ALSO set a media-type filter (e.g. "photos only"), photos-
// first strategy has nothing left to sequence -- one pass, untouched.
func TestUploadPasses_PhotosFirst_AlreadyNarrowed_SinglePass(t *testing.T) {
	cfg := config.Defaults()
	cfg.SyncStrategy = config.SyncStrategyPhotosFirst
	cfg.MediaTypeFilter = extensions.KindVideo

	passes := uploadPasses(cfg)
	if len(passes) != 1 {
		t.Fatalf("uploadPasses = %d passes, want 1 (MediaTypeFilter already narrowed)", len(passes))
	}
	if passes[0].MediaTypeFilter != extensions.KindVideo {
		t.Errorf("passes[0].MediaTypeFilter = %v, want unchanged KindVideo", passes[0].MediaTypeFilter)
	}
}

// TestUploadScopeFor_FolderByFolder_StaysScopedToOneFolder is the default:
// even with SourceFolders configured, folder_by_folder must never widen
// the upload phase.
func TestUploadScopeFor_FolderByFolder_StaysScopedToOneFolder(t *testing.T) {
	cfg := config.Defaults()
	cfg.SourceFolders = []string{"/lib/a", "/lib/b", "/lib/c"}
	got := UploadScopeFor(cfg, "/lib/b")
	want := []string{"/lib/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UploadScopeFor = %v, want %v", got, want)
	}
}

// TestUploadScopeFor_GlobalStrategies_WidenToSourceFolders covers two
// failures of the global sync strategies, where smallest-first and
// photos-first both appeared to be ignored: they must consider every
// TOP-LEVEL configured folder, not just the one that triggered this
// cycle. Deliberately cfg.SourceFolders (the few roots actually
// configured), NOT a fully-expanded per-leaf-subfolder list -- an earlier
// version of this fix used the expanded list and caused a serious
// regression: db.ListPendingUnder builds one OR'd LIKE clause per folder
// passed in, and a query with hundreds of clauses (one per leaf
// subfolder in a real library) against tens of thousands of pending rows
// could run for MINUTES with the dashboard showing nothing at all while
// it churned and the queue appeared untouched. A handful of top-level
// root prefixes match exactly the
// same files (SQL LIKE's own `%` already covers everything nested
// underneath) at a fraction of the cost.
func TestUploadScopeFor_GlobalStrategies_WidenToSourceFolders(t *testing.T) {
	roots := []string{"/lib/a", "/lib/b", "/lib/c"}
	for _, strategy := range []string{config.SyncStrategySmallestFirst, config.SyncStrategyPhotosFirst} {
		cfg := config.Defaults()
		cfg.SyncStrategy = strategy
		cfg.SourceFolders = roots
		got := UploadScopeFor(cfg, "/lib/b/some/leaf/subfolder")
		if !reflect.DeepEqual(got, roots) {
			t.Errorf("UploadScopeFor(%s) = %v, want %v (every configured root, not just the trigger folder)", strategy, got, roots)
		}
	}
}

// TestUploadScopeFor_NoSourceFoldersConfigured_FallsBackToOneFolder
// guards against an empty upload scope (which would mean "match
// everything") if cfg.SourceFolders is somehow empty despite a global
// strategy being selected.
func TestUploadScopeFor_NoSourceFoldersConfigured_FallsBackToOneFolder(t *testing.T) {
	cfg := config.Defaults()
	cfg.SyncStrategy = config.SyncStrategySmallestFirst
	got := UploadScopeFor(cfg, "/lib/b")
	want := []string{"/lib/b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UploadScopeFor = %v, want %v", got, want)
	}
}
