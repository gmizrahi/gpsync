package statedb

import "testing"

// seedSuperseded registers a file the way a scan does: a ledger row plus the
// scan-cache row for the path it was found at.
func seedSuperseded(t *testing.T, db *DB, sha, path string) {
	t.Helper()
	if err := db.EnsurePending(sha, 10, "image/jpeg", path, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFileSeen(path, sha, 0, 10); err != nil {
		t.Fatal(err)
	}
}

// Issue #22. Editing a photo in place keeps its path and changes its hash,
// so the next scan rewrites that path's cache row and leaves the old hash's
// ledger row behind, still naming a path that is no longer its content.
func TestSupersededRows_EditedInPlace_IsFound(t *testing.T) {
	db := openTestDB(t)
	const path = "/photos/2023/20230305_103022.jpg"
	seedSuperseded(t, db, "sha-original", path)
	// The edit: same path, a new hash at it.
	if err := db.UpsertFileSeen(path, "sha-edited", 0, 11); err != nil {
		t.Fatal(err)
	}

	got, err := db.SupersededRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("SupersededRows() = %+v, want the one stale row", got)
	}
	if got[0].SHA256 != "sha-original" || got[0].CurrentSHA256 != "sha-edited" {
		t.Errorf("row = %+v, want sha-original superseded by sha-edited", got[0])
	}
	if got[0].FirstSourcePath != path {
		t.Errorf("path = %q, want %q", got[0].FirstSourcePath, path)
	}
}

// A file that was MOVED keeps its hash under a new path. Its content is
// still held, so the row still describes something real -- even though the
// old path has since been filled by different content.
func TestSupersededRows_ContentStillHeldElsewhere_IsNotSuperseded(t *testing.T) {
	db := openTestDB(t)
	const path = "/photos/2023/20230305_103022.jpg"
	seedSuperseded(t, db, "sha-original", path)
	// Moved away, and something else took the old name.
	if err := db.UpsertFileSeen("/photos/sorted/20230305_103022.jpg", "sha-original", 0, 10); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFileSeen(path, "sha-other", 0, 11); err != nil {
		t.Fatal(err)
	}

	got, err := db.SupersededRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("SupersededRows() = %+v, want none: that content is still on disk elsewhere", got)
	}
}

// A DELETED file is not superseded. A scan prunes its cache row, which is
// what the join looks for -- deletion belongs to the missing-source-file
// check, which never forgets a row on its own, so an unplugged drive loses
// nothing.
func TestSupersededRows_DeletedFile_IsNotSuperseded(t *testing.T) {
	db := openTestDB(t)
	const path = "/photos/2023/20230305_103022.jpg"
	seedSuperseded(t, db, "sha-original", path)
	if err := db.DeleteFileSeen(path); err != nil {
		t.Fatal(err)
	}

	got, err := db.SupersededRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("SupersededRows() = %+v, want none: a deleted file is missing, not superseded", got)
	}
}
