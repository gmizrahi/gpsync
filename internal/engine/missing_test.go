package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gmizrahi/gpsync/internal/scanner"
	"github.com/gmizrahi/gpsync/internal/statedb"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func scanFolder(t *testing.T, db *statedb.DB, folder string) scanner.ScanSummary {
	t.Helper()
	sum, err := scanner.ScanFolders(db, []string{folder}, "manual_scan", 2, false, false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func resolve(t *testing.T, db *statedb.DB, roots ...string) MissingResolveSummary {
	t.Helper()
	sum, err := ResolveMissingFiles(db, roots, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func missingCounts(t *testing.T, db *statedb.DB) statedb.MissingCounts {
	t.Helper()
	c, err := db.MissingCounts()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A file moved to another folder is the same content at a new path: it must
// be repointed, never forgotten. Real library: "2024_03 Spring\IMG-...-WA0032.jpg"
// turned up, renamed, as "2024_03 Festival\IMG-...-WA0004.jpg".
func TestResolveMissing_MovedFileIsRepointed(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	oldPath := filepath.Join(root, "2024_03 Spring", "IMG-0032.jpg")
	newPath := filepath.Join(root, "2024_03 Festival", "IMG-0004.jpg")
	writeTestFile(t, oldPath, "same bytes")
	writeTestFile(t, filepath.Join(root, "2024_03 Spring", "stays.jpg"), "stays")
	scanFolder(t, db, root)

	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatal(err)
	}
	if sum := scanFolder(t, db, root); sum.Missing != 1 {
		t.Fatalf("scan flagged %d entries, want 1 (the old path)", sum.Missing)
	}

	res := resolve(t, db, root)
	if res.Relocated != 1 || res.Confirmed != 0 {
		t.Fatalf("got %+v, want 1 relocated and 0 confirmed", res)
	}
	if c := missingCounts(t, db); c.Flagged != 0 || c.Confirmed != 0 {
		t.Errorf("missing counts after relocation = %+v, want zero", c)
	}
	rows, _ := db.UploadsUnder([]string{filepath.Dir(newPath)})
	if len(rows) != 1 || rows[0].Path != newPath {
		t.Errorf("entries under the new folder = %+v, want one pointing at %s", rows, newPath)
	}
}

// The case that ruled out "confirm after every folder was scanned once":
// the destination folder was last scanned BEFORE the move, so its copy is not
// hashed yet. Confirming would let recheck --missing forget the entry and the
// next scan re-upload the file as a duplicate. It must wait instead.
func TestResolveMissing_UnhashedSameSizeFileDefers(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	writeTestFile(t, filepath.Join(a, "moved.jpg"), "moved content")
	writeTestFile(t, filepath.Join(b, "other.jpg"), "other")
	scanFolder(t, db, root)

	if err := os.Rename(filepath.Join(a, "moved.jpg"), filepath.Join(b, "moved.jpg")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(a, "keep.jpg"), "keep")
	scanFolder(t, db, a) // only the source folder; b is not rescanned

	res := resolve(t, db, root)
	if res.Confirmed != 0 || res.Deferred != 1 {
		t.Fatalf("got %+v, want 0 confirmed and 1 deferred", res)
	}

	scanFolder(t, db, b)
	if res := resolve(t, db, root); res.Relocated != 1 {
		t.Errorf("after b is scanned: %+v, want 1 relocated", res)
	}
}

func TestResolveMissing_DeletedFileIsConfirmed(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	victim := filepath.Join(root, "a", "bad-quality.jpg")
	writeTestFile(t, victim, "blurry")
	writeTestFile(t, filepath.Join(root, "a", "good.jpg"), "sharp")
	scanFolder(t, db, root)

	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	scanFolder(t, db, root)
	if c := missingCounts(t, db); c.Flagged != 1 || c.Confirmed != 0 {
		t.Fatalf("after the scan: %+v, want 1 flagged, 0 confirmed -- a scan alone never confirms", c)
	}

	if res := resolve(t, db, root); res.Confirmed != 1 {
		t.Fatalf("got %+v, want 1 confirmed", res)
	}
	gone, err := db.ConfirmedMissing()
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 1 || gone[0].Path != victim {
		t.Errorf("confirmed-missing = %+v, want just %s", gone, victim)
	}
}

// A reattached drive or a restored file must undo a confirmation.
func TestResolveMissing_ReturnedFileUndoesConfirmation(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	f := filepath.Join(root, "a", "back.jpg")
	writeTestFile(t, f, "back")
	writeTestFile(t, filepath.Join(root, "a", "other.jpg"), "other")
	scanFolder(t, db, root)
	os.Remove(f)
	scanFolder(t, db, root)
	if res := resolve(t, db, root); res.Confirmed != 1 {
		t.Fatalf("setup: %+v, want 1 confirmed", res)
	}

	writeTestFile(t, f, "back")
	if res := resolve(t, db, root); res.Reappeared != 1 {
		t.Fatalf("got %+v, want 1 reappeared", res)
	}
	if c := missingCounts(t, db); c.Flagged != 0 || c.Confirmed != 0 {
		t.Errorf("missing counts = %+v, want zero", c)
	}
}

// An unavailable source folder could be where a file went: decide nothing.
func TestResolveMissing_UnavailableSourceFolderDecidesNothing(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	f := filepath.Join(root, "a", "gone.jpg")
	writeTestFile(t, f, "gone")
	writeTestFile(t, filepath.Join(root, "a", "other.jpg"), "other")
	scanFolder(t, db, root)
	os.Remove(f)
	scanFolder(t, db, root)

	res := resolve(t, db, root, filepath.Join(t.TempDir(), "unplugged-drive"))
	if !res.Skipped || res.Confirmed != 0 {
		t.Fatalf("got %+v, want skipped with nothing confirmed", res)
	}
}

// Entries outside the configured source folders are left alone.
func TestResolveMissing_OutsideSourceFoldersLeftAlone(t *testing.T) {
	db := openTestDB(t)
	elsewhere := t.TempDir()
	f := filepath.Join(elsewhere, "x", "gone.jpg")
	writeTestFile(t, f, "gone")
	writeTestFile(t, filepath.Join(elsewhere, "x", "other.jpg"), "other")
	scanFolder(t, db, elsewhere)
	os.Remove(f)
	scanFolder(t, db, elsewhere)

	res := resolve(t, db, t.TempDir())
	if res.Confirmed != 0 || res.Outside != 1 {
		t.Errorf("got %+v, want 0 confirmed and 1 outside", res)
	}
}

// The worst failure mode: an unavailable drive looking like mass deletion.
func TestResolveMissing_RefusesWhenMuchOfTheLibraryLooksMissing(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	dir := filepath.Join(root, "lib")
	for i := 0; i < missingConfirmFloor+50; i++ {
		writeTestFile(t, filepath.Join(dir, fmt.Sprintf("f%04d.jpg", i)), fmt.Sprintf("unique-%d", i))
	}
	writeTestFile(t, filepath.Join(root, "keep", "anchor.jpg"), "anchor")
	scanFolder(t, db, root)

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		os.Remove(filepath.Join(dir, e.Name()))
	}
	scanFolder(t, db, root)

	res := resolve(t, db, root)
	if !res.Skipped || res.Confirmed != 0 {
		t.Fatalf("got %+v, want skipped with nothing confirmed", res)
	}
	if c := missingCounts(t, db); c.Confirmed != 0 {
		t.Errorf("%d entries confirmed despite the refusal", c.Confirmed)
	}
}

// A confirmed entry that can no longer be decided -- a same-size file shows
// up unhashed -- must lose its confirmation, or recheck --missing would
// forget what may be a moved file.
func TestResolveMissing_UndecidableAgainWithdrawsConfirmation(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	f := filepath.Join(root, "a", "photo.jpg")
	writeTestFile(t, f, "twelve bytes")
	writeTestFile(t, filepath.Join(root, "a", "other.jpg"), "other")
	scanFolder(t, db, root)
	os.Remove(f)
	scanFolder(t, db, root)
	if res := resolve(t, db, root); res.Confirmed != 1 {
		t.Fatalf("setup: %+v, want 1 confirmed", res)
	}

	writeTestFile(t, filepath.Join(root, "b", "renamed.jpg"), "twelve bytes") // not scanned yet
	if res := resolve(t, db, root); res.Deferred != 1 {
		t.Fatalf("got %+v, want 1 deferred", res)
	}
	if c := missingCounts(t, db); c.Confirmed != 0 || c.Flagged != 1 {
		t.Errorf("missing counts = %+v, want 0 confirmed and 1 flagged", c)
	}
}

// scanUnits scans the way gpsync scan, sync and the tray do: one ScanFolders
// call per folder that directly contains files.
func scanUnits(t *testing.T, db *statedb.DB, root string) {
	t.Helper()
	units, err := DiscoverSyncUnits([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range units {
		scanFolder(t, db, u)
	}
}

// A deleted folder is never scanned again, and its parent usually holds no
// files of its own, so no per-folder scan covers the old path. Review
// finding: its entries were never flagged.
func TestResolveMissing_DeletedFolderIsConfirmed(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "2024", "2024_03 Spring", "a.jpg"), "aaa")
	writeTestFile(t, filepath.Join(root, "2024", "2024_04 Other", "b.jpg"), "bbb")
	scanUnits(t, db, root)

	if err := os.RemoveAll(filepath.Join(root, "2024", "2024_03 Spring")); err != nil {
		t.Fatal(err)
	}
	scanUnits(t, db, root)
	res := resolve(t, db, root)
	if res.NewlyFlagged != 1 || res.Confirmed != 1 {
		t.Fatalf("got %+v, want the deleted folder's file flagged and confirmed", res)
	}
}

// Renaming a folder must repoint its entries. Review finding: the ledger kept
// the old folder name forever.
func TestResolveMissing_RenamedFolderIsRepointed(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "2024", "2024_03 Spring", "a.jpg"), "aaa")
	writeTestFile(t, filepath.Join(root, "2024", "2024_04 Other", "b.jpg"), "bbb")
	scanUnits(t, db, root)

	renamed := filepath.Join(root, "2024", "2024_03 Festival")
	if err := os.Rename(filepath.Join(root, "2024", "2024_03 Spring"), renamed); err != nil {
		t.Fatal(err)
	}
	scanUnits(t, db, root)
	res := resolve(t, db, root)
	if res.Relocated != 1 || res.Confirmed != 0 {
		t.Fatalf("got %+v, want 1 relocated and 0 confirmed", res)
	}
	rows, err := db.UploadsUnder([]string{renamed})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Path != filepath.Join(renamed, "a.jpg") {
		t.Errorf("entries under the new folder name = %+v, want a.jpg repointed there", rows)
	}
}

// A file a scan never hashes (here an unsupported .xmp sidecar) cannot be a
// moved ledger entry, so a same-size one must not defer a deletion. Review
// finding: it deferred on every pass, and the tray walked the whole library
// on every heartbeat as a result.
func TestResolveMissing_UnhashableSameSizeFileDoesNotDefer(t *testing.T) {
	db := openTestDB(t)
	root := t.TempDir()
	photo := filepath.Join(root, "a", "photo.jpg")
	writeTestFile(t, photo, "0123456789")
	writeTestFile(t, filepath.Join(root, "a", "other.jpg"), "other")
	writeTestFile(t, filepath.Join(root, "a", "photo.xmp"), "abcdefghij") // same size, unsupported
	scanFolder(t, db, root)
	if err := os.Remove(photo); err != nil {
		t.Fatal(err)
	}
	scanFolder(t, db, root)

	if res := resolve(t, db, root); res.Confirmed != 1 || res.Deferred != 0 {
		t.Fatalf("got %+v, want 1 confirmed and 0 deferred", res)
	}
}
