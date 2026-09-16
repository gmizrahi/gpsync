package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEstimateBatchTotals_CountsFilesAndFlagsAlreadySyncedFromCache proves
// estimateBatchTotals' no-hashing pre-scan: it must count every file's
// size across all given folders (even ones never touched by a prior scan),
// and separately identify -- purely from the files_seen cache, no
// hashing/network -- which of those files are already 'uploaded', since
// that's exactly the same cache-hit check ScanFolders itself uses to skip
// re-hashing unchanged files.
func TestEstimateBatchTotals_CountsFilesAndFlagsAlreadySyncedFromCache(t *testing.T) {
	t.Setenv("GPSYNC_STATE_DIR", t.TempDir())
	db, err := statedb.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	root := t.TempDir()
	syncedPath := filepath.Join(root, "folderA", "already_synced.jpg")
	pendingPath := filepath.Join(root, "folderA", "not_yet_scanned.jpg")
	otherFolderPath := filepath.Join(root, "folderB", "another.jpg")
	touch(t, syncedPath)
	touch(t, pendingPath)
	touch(t, otherFolderPath)

	// Simulate a prior real scan+upload of syncedPath: files_seen cache
	// entry matching its current (path, mtime, size), and its content hash
	// already marked 'uploaded' in the ledger. pendingPath and
	// otherFolderPath are left completely untouched -- as if never scanned.
	info, err := os.Stat(syncedPath)
	if err != nil {
		t.Fatal(err)
	}
	const hash = "deadbeef"
	if err := db.EnsurePending(hash, info.Size(), "image/jpeg", syncedPath, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkUploaded(hash, "media-item-1", ""); err != nil {
		t.Fatal(err)
	}
	mtime := float64(info.ModTime().UnixNano()) / 1e9
	if err := db.UpsertFileSeen(syncedPath, hash, mtime, info.Size()); err != nil {
		t.Fatal(err)
	}

	units := []string{filepath.Join(root, "folderA"), filepath.Join(root, "folderB")}
	filesTotal, bytesTotal, alreadySynced, alreadySyncedBytes, err := estimateBatchTotals(db, units)
	if err != nil {
		t.Fatal(err)
	}

	if filesTotal != 3 {
		t.Errorf("filesTotal = %d, want 3 (all files across both folders, scanned or not)", filesTotal)
	}
	wantBytes := int64(1) * 3 // touch() writes 1 byte per file
	if bytesTotal != wantBytes {
		t.Errorf("bytesTotal = %d, want %d", bytesTotal, wantBytes)
	}
	if alreadySynced != 1 {
		t.Errorf("alreadySynced = %d, want 1 (only syncedPath has a matching cache hit + 'uploaded' status)", alreadySynced)
	}
	if alreadySyncedBytes != info.Size() {
		t.Errorf("alreadySyncedBytes = %d, want %d", alreadySyncedBytes, info.Size())
	}
}
