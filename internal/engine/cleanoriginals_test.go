package engine

import (
	"path/filepath"
	"testing"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

func seedQueued(t *testing.T, db *statedb.DB, sha, path string) {
	t.Helper()
	if err := db.EnsurePending(sha, 10, "image/jpeg", path, nil); err != nil {
		t.Fatal(err)
	}
	// The query JOINs files_seen, so seeding only uploads finds nothing --
	// the same gap this project hit once on OriginalsFolderRowsUploaded.
	if err := db.UpsertFileSeen(path, sha, 0, 10); err != nil {
		t.Fatal(err)
	}
}

// The SQL pre-filter is a coarse LIKE on the path; only an immediate
// parent named "originals" is a real match, so a path merely containing
// the word somewhere must not be swept up.
func TestCleanOriginals_OnlyImmediateParentCounts(t *testing.T) {
	db := openTestDB(t)
	real := filepath.Join("C:", "Photos", "2013", "trip", "originals", "IMG_1.jpg")
	notReal := filepath.Join("C:", "Photos", "originals-archive", "IMG_2.jpg")
	deeper := filepath.Join("C:", "Photos", "originals", "sub", "IMG_3.jpg")
	seedQueued(t, db, "h-real", real)
	seedQueued(t, db, "h-notreal", notReal)
	seedQueued(t, db, "h-deeper", deeper)

	sum, err := CleanOriginals(db, false)
	if err != nil {
		t.Fatalf("CleanOriginals: %v", err)
	}
	if sum.Matched != 1 {
		t.Fatalf("Matched = %d, want 1 (only the immediate-parent case)", sum.Matched)
	}
	if sum.Items[0].FirstSourcePath != real {
		t.Errorf("matched %q, want %q", sum.Items[0].FirstSourcePath, real)
	}
}

// The dry run is what the dashboard button's count comes from.
func TestCleanOriginals_DryRunCountsButWritesNothing(t *testing.T) {
	db := openTestDB(t)
	p := filepath.Join("C:", "Photos", "2013", "originals", "IMG_1.jpg")
	seedQueued(t, db, "h1", p)

	dry, err := CleanOriginals(db, false)
	if err != nil {
		t.Fatalf("dry: %v", err)
	}
	if dry.Matched != 1 {
		t.Fatalf("dry Matched = %d, want 1", dry.Matched)
	}
	u, err := db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status == "needs_review" {
		t.Fatal("the dry run migrated the row")
	}

	wet, err := CleanOriginals(db, true)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if wet.Matched != dry.Matched {
		t.Errorf("commit matched %d, dry said %d", wet.Matched, dry.Matched)
	}
	u, err = db.GetUpload("h1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "needs_review" {
		t.Errorf("status = %q, want needs_review", u.Status)
	}
}
