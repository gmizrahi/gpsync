package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fatih/color"

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
// flow or network round trip (oauth2.ReuseTokenSource only refreshes when
// the token is actually used AND expired -- this one's expiry is far in the
// future). Needed to exercise syncOneFolder's upload-phase loop logic, as
// opposed to just proving uploader.New fails without credentials like this
// file's other tests. Restores the real paths via t.Cleanup. Mirrors
// internal/engine/cycle_test.go's identical helper.
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

// TestSyncOneFolder_AlreadySyncedFolder_CollapsesToOneLine is the fix for
// a real complaint: gpsync watch's backlog drain revisits every folder in a
// large already-caught-up library, and the full scan+upload dashboard for
// a folder with genuinely nothing to do (the overwhelming common case) was
// 6-9 lines of noise repeated thousands of times. A folder with no
// already-pending work and nothing new to hash must collapse to one
// summary line -- and, critically, must return BEFORE ever reaching the
// upload phase (which would need real OAuth credentials this test doesn't
// have) rather than just printing less.
func TestSyncOneFolder_AlreadySyncedFolder_CollapsesToOneLine(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	db := openInfoTestDB(t)
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

	var synced, total, failedPerm, failedRetry int
	var cycleErr error
	out := captureStdout(t, func() {
		synced, total, failedPerm, failedRetry, cycleErr = syncOneFolder(db, config.Defaults(), dir, false, nil, conc, breaker)
	})

	if cycleErr != nil {
		t.Fatalf("syncOneFolder = %v, want nil -- an already-synced folder must never reach the upload phase (no OAuth client in this test)", cycleErr)
	}
	if synced != 1 || total != 1 || failedPerm != 0 || failedRetry != 0 {
		t.Errorf("counts = synced=%d total=%d failedPerm=%d failedRetry=%d, want 1/1/0/0", synced, total, failedPerm, failedRetry)
	}
	if !strings.Contains(out, "already synced") {
		t.Errorf("expected an 'already synced' summary line, got:\n%s", out)
	}
	for _, noise := range []string{"[1/2] Scanning", "[2/2] Uploading", "Transferred:", "fully synced."} {
		if strings.Contains(out, noise) {
			t.Errorf("output should collapse to one line, but still contains %q:\n%s", noise, out)
		}
	}
}

// TestSyncOneFolder_PendingWork_StaysVerboseEvenIfScanFindsNothingNew
// guards the safety check: a folder can have real upload work outstanding
// from an earlier interrupted run even when THIS scan finds nothing new to
// hash -- that must never be silently collapsed away, since there's
// genuine work the user needs visibility into (and which the caller needs
// to actually attempt).
func TestSyncOneFolder_PendingWork_StaysVerboseEvenIfScanFindsNothingNew(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	db := openInfoTestDB(t)
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
	// Deliberately NOT marked uploaded -- this row is still 'pending',
	// exactly the "scanned before, never got uploaded" case.

	conc := quota.NewAdaptiveConcurrency(1)
	breaker := uploader.NewCircuitBreakerState()

	var cycleErr error
	out := captureStdout(t, func() {
		_, _, _, _, cycleErr = syncOneFolder(db, config.Defaults(), dir, false, nil, conc, breaker)
	})

	// This test environment has no OAuth client configured, so
	// uploader.New fails immediately (see TestRunFolderCycle_
	// PendingWork_StillAttemptsUpload's own doc comment for the same
	// reasoning in internal/engine) -- BEFORE the concurrent scan's own
	// background goroutine necessarily gets a chance to print its summary
	// line. What actually matters -- and is robust regardless of that
	// race -- is that the upload phase was genuinely ATTEMPTED (not
	// silently skipped the way a real "already synced" cycle would): the
	// returned error must be the upload-setup failure, not nil.
	if cycleErr == nil || !strings.Contains(cycleErr.Error(), "upload failed") {
		t.Errorf("syncOneFolder error = %v, want an 'upload failed' error -- pending work must actually be attempted, not silently skipped", cycleErr)
	}
	if strings.Contains(out, "already synced") {
		t.Errorf("must not report 'already synced' when there's genuine pending work:\n%s", out)
	}
}

// TestSyncOneFolder_AllPendingExcludedByMediaTypeFilter_StopsInsteadOfSpinning
// covers the bug where `gpsync sync --media-type photos
// <folder>` (or --media-type videos) against a folder that already has
// pending rows of the OTHER kind (from an earlier unfiltered scan) spun
// this loop forever -- Run() returns cleanly with SkippedMediaType>0 and no
// error, so `pending` at the top of the loop was identical lap after lap,
// with nothing checking ctx cancellation in that path. Uses fake OAuth
// credentials (see withFakeOAuthCredentials) specifically so the upload
// phase's own filtering logic actually runs; no real HTTP request is made,
// since every row is excluded before any network call.
func TestSyncOneFolder_AllPendingExcludedByMediaTypeFilter_StopsInsteadOfSpinning(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	db := openInfoTestDB(t)
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

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, _, _, _, cycleErr := syncOneFolder(db, cfg, dir, false, nil, conc, breaker)
		done <- result{cycleErr}
	}()

	select {
	case r := <-done:
		var abortErr *uploader.AbortError
		if !errors.As(r.err, &abortErr) {
			t.Fatalf("syncOneFolder error = %v, want an *uploader.AbortError", r.err)
		}
		if abortErr.Reason != uploader.AbortNoEligibleFiles {
			t.Errorf("AbortError.Reason = %q, want %q", abortErr.Reason, uploader.AbortNoEligibleFiles)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("syncOneFolder did not return within 5s -- it is spinning forever, exactly the bug this test guards against")
	}
}
