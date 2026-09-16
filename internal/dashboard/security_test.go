package dashboard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gmizrahi/gpsync/internal/config"
)

// These three functions are the server-side re-derivation this dashboard
// relies on instead of trusting a client-supplied path. A code review
// found an arbitrary-file-move vulnerability in this exact area, which a
// dashboard reachable beyond 127.0.0.1 makes reachable too.

func TestDupGroupsFiltered_ExcludesGroupsWithFewerThanTwoSurvivingPaths(t *testing.T) {
	db := openTestDB(t)
	dir := t.TempDir()

	a := filepath.Join(dir, "a.jpg")
	b := filepath.Join(dir, "b.jpg")
	gone := filepath.Join(dir, "gone.jpg") // never actually written to disk

	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("content"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	mustNoErr(t, db.EnsurePending("hash-real", 7, "image/jpeg", a, nil))
	mustNoErr(t, db.UpsertFileSeen(a, "hash-real", 0, 7))
	mustNoErr(t, db.UpsertFileSeen(b, "hash-real", 0, 7))

	mustNoErr(t, db.EnsurePending("hash-orphan", 7, "image/jpeg", gone, nil))
	// hash-orphan gets two files_seen rows too, but NEITHER exists on disk
	// -- dupGroupsFiltered must drop this group entirely, not surface a
	// group of vanished paths as still resolvable.
	other := filepath.Join(dir, "also-gone.jpg")
	mustNoErr(t, db.UpsertFileSeen(gone, "hash-orphan", 0, 7))
	mustNoErr(t, db.UpsertFileSeen(other, "hash-orphan", 0, 7))

	groups, err := dupGroupsFiltered(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1 (only hash-real, with both files really on disk)", len(groups))
	}
	if groups[0].SHA256 != "hash-real" {
		t.Errorf("groups[0].SHA256 = %q, want hash-real", groups[0].SHA256)
	}
	if len(groups[0].Paths) != 2 {
		t.Errorf("len(groups[0].Paths) = %d, want 2", len(groups[0].Paths))
	}
}

func TestOriginalsPathIsKnown_RejectsAnUnrelatedPath(t *testing.T) {
	db := openTestDB(t)
	source := filepath.Join(t.TempDir(), "originals", "img.jpg")
	mustNoErr(t, db.EnsureNeedsReview("hash-1", 100, "image/jpeg", source, nil))

	known, err := originalsPathIsKnown(db, "hash-1", source)
	if err != nil {
		t.Fatal(err)
	}
	if !known {
		t.Error("the review item's own FirstSourcePath must be known")
	}

	known, err = originalsPathIsKnown(db, "hash-1", "/etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	if known {
		t.Error("an unrelated path must NOT be reported as known -- this gates handleOriginalsThumb/handleOriginalsRaw's file reads")
	}
}

func TestOriginalsPathIsKnown_UnknownHash_NeverKnown(t *testing.T) {
	db := openTestDB(t)
	known, err := originalsPathIsKnown(db, "no-such-hash", "/anything")
	if err != nil {
		t.Fatal(err)
	}
	if known {
		t.Error("a hash with no needs_review row must never report a path as known")
	}
}

func TestBackupFileFromRequest_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "gpsync-backup-2026-01-01.zip")
	if err := os.WriteFile(real, []byte("zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.BackupDir = dir

	path, ok := backupFileFromRequest(cfg, "gpsync-backup-2026-01-01.zip")
	if !ok || path != real {
		t.Errorf("backupFileFromRequest(real basename) = (%q, %v), want (%q, true)", path, ok, real)
	}

	for _, traversal := range []string{
		"../../../etc/passwd",
		"..\\..\\windows\\system32\\config",
		"/etc/passwd",
		"",
		".",
	} {
		if _, ok := backupFileFromRequest(cfg, traversal); ok {
			t.Errorf("backupFileFromRequest(%q) = ok, want rejected -- only a real basename inside cfg.BackupDir may resolve", traversal)
		}
	}
}
