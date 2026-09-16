package main

import (
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/extensions"
	"github.com/gmizrahi/gpsync/internal/scanner"
)

// TestAutoCleanExcluded_RemovesRowsForNewlyUnsupportedExtension is the fix
// for the exact real-world case this feature was built for: a user adds an
// extension to extra_unsupported_extensions in config.toml, and any
// pending/failed_permanent ledger row for it needs to disappear on the
// very next scan/upload/sync -- without a separate manual `gpsync recheck
// --unsupported` step.
func TestAutoCleanExcluded_RemovesRowsForNewlyUnsupportedExtension(t *testing.T) {
	t.Cleanup(func() { extensions.ApplyUserOverrides(nil, nil, nil) })
	db := openInfoTestDB(t)

	// .weird isn't in any built-in table -- Unknown until the config
	// override below makes it Unsupported.
	mustNoErr(t, db.EnsurePending("h-weird", 1, "application/octet-stream", "/lib/2024/oddball.weird", nil))
	mustNoErr(t, db.EnsurePending("h-jpg", 1, "image/jpeg", "/lib/2024/photo.jpg", nil))

	// Not yet configured as unsupported -- nothing to clean.
	n, err := autoCleanExcluded(db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("autoCleanExcluded before configuring the extension = %d, want 0", n)
	}

	extensions.ApplyUserOverrides(nil, nil, []string{"weird"})

	n, err = autoCleanExcluded(db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("autoCleanExcluded = %d, want 1", n)
	}
	if u, err := db.GetUpload("h-weird"); err != nil || u != nil {
		t.Errorf("expected h-weird removed: u=%+v err=%v", u, err)
	}
	if u, err := db.GetUpload("h-jpg"); err != nil || u == nil {
		t.Errorf("h-jpg (unrelated) must be untouched: u=%+v err=%v", u, err)
	}
}

// TestAutoCleanExcluded_RemovesRowsForNewlyIgnoredFileName covers the
// ignore-list half: a filename added to extra_ignored_file_names should be
// cleaned up the same way as a newly-unsupported extension.
func TestAutoCleanExcluded_RemovesRowsForNewlyIgnoredFileName(t *testing.T) {
	t.Cleanup(func() { scanner.ApplyUserIgnoreOverrides(nil, nil) })
	db := openInfoTestDB(t)

	mustNoErr(t, db.EnsurePending("h-junk", 1, "application/octet-stream", "/lib/2024/export.log", nil))

	scanner.ApplyUserIgnoreOverrides([]string{"export.log"}, nil)

	n, err := autoCleanExcluded(db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("autoCleanExcluded = %d, want 1", n)
	}
	if u, err := db.GetUpload("h-junk"); err != nil || u != nil {
		t.Errorf("expected h-junk removed: u=%+v err=%v", u, err)
	}
}

// TestAutoCleanExcluded_RemovesRowsUnderNewlyIgnoredDir proves the
// directory-ignore case, which needs to check every ancestor directory
// name, not just the immediate parent.
func TestAutoCleanExcluded_RemovesRowsUnderNewlyIgnoredDir(t *testing.T) {
	t.Cleanup(func() { scanner.ApplyUserIgnoreOverrides(nil, nil) })
	db := openInfoTestDB(t)

	mustNoErr(t, db.EnsurePending("h-deep", 1, "image/jpeg", "/lib/2024/CameraSync/2024_01/photo.jpg", nil))

	scanner.ApplyUserIgnoreOverrides(nil, []string{"CameraSync"})

	n, err := autoCleanExcluded(db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("autoCleanExcluded = %d, want 1", n)
	}
}

// TestAutoCleanExcluded_LeavesFailedRetryableAlone proves the scope is
// exactly pending + failed_permanent -- a failed_retryable row (a real
// transient error, not an extension classification) must never be touched
// by this pass.
func TestAutoCleanExcluded_LeavesFailedRetryableAlone(t *testing.T) {
	t.Cleanup(func() { extensions.ApplyUserOverrides(nil, nil, nil) })
	db := openInfoTestDB(t)

	mustNoErr(t, db.EnsurePending("h-retry", 1, "application/octet-stream", "/lib/2024/slides.ppt", nil))
	mustNoErr(t, db.MarkFailed("h-retry", false, "THROTTLE", "slow down"))

	extensions.ApplyUserOverrides(nil, nil, []string{"ppt"})

	n, err := autoCleanExcluded(db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("autoCleanExcluded touched a failed_retryable row: removed %d", n)
	}
	u, err := db.GetUpload("h-retry")
	if err != nil || u == nil || u.Status != "failed_retryable" {
		t.Errorf("h-retry = %+v, err=%v, want unchanged failed_retryable", u, err)
	}
}

// TestReportAutoClean_SilentWhenNothingToClean matters for a "data and
// visibility person": a notice on every single ordinary run, when there's
// usually nothing to clean, would just be noise to filter out. Only an
// actual cleanup should print anything.
func TestReportAutoClean_SilentWhenNothingToClean(t *testing.T) {
	db := openInfoTestDB(t)
	mustNoErr(t, db.EnsurePending("h-jpg", 1, "image/jpeg", "/lib/2024/photo.jpg", nil))

	out := captureStdout(t, func() { reportAutoClean(db) })
	if out != "" {
		t.Errorf("expected no output when nothing was cleaned, got: %q", out)
	}
}

func TestReportAutoClean_ReportsWhenSomethingWasCleaned(t *testing.T) {
	t.Cleanup(func() { extensions.ApplyUserOverrides(nil, nil, nil) })
	db := openInfoTestDB(t)
	mustNoErr(t, db.EnsurePending("h-ppt", 1, "application/octet-stream", "/lib/2024/slides.ppt", nil))
	extensions.ApplyUserOverrides(nil, nil, []string{"ppt"})

	out := captureStdout(t, func() { reportAutoClean(db) })
	if !strings.Contains(out, "cleaned:") || !strings.Contains(out, "1") {
		t.Errorf("expected a cleanup notice naming 1 record, got: %q", out)
	}
}
