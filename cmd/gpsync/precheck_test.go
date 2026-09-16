package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/fatih/color"

	"github.com/gmizrahi/gpsync/internal/hashing"
)

// TestPrecheckFolder_SortsNewFromAlreadyBackedUp is the core case: a
// phone-dump folder with some content the ledger has never seen (left in
// place) and some that's already backed up under a different path (moved
// out of the way so it doesn't get copied into the library too).
func TestPrecheckFolder_SortsNewFromAlreadyBackedUp(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	db := openInfoTestDB(t)
	src := t.TempDir()
	dupes := t.TempDir()

	newFile := filepath.Join(src, "IMG_0100.jpg")
	writeTestFileContent(t, newFile, "brand-new-content")

	dupeFile := filepath.Join(src, "IMG_0042.jpg")
	writeTestFileContent(t, dupeFile, "already-backed-up-content")
	sum, err := hashing.SHA256File(dupeFile)
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(t.TempDir(), "2024", "IMG_0042.jpg")
	mustNoErr(t, db.EnsurePending(sum, 25, "image/jpeg", original, nil))
	mustNoErr(t, db.MarkUploaded(sum, "AGj1abc", ""))

	var out bytes.Buffer
	res, err := precheckFolder(db, src, dupes, false, &out)
	if err != nil {
		t.Fatal(err)
	}

	if res.newFiles != 1 || res.dupeFiles != 1 {
		t.Fatalf("got newFiles=%d dupeFiles=%d, want 1/1:\n%s", res.newFiles, res.dupeFiles, out.String())
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Errorf("new file should be left in place: %v", err)
	}
	if _, err := os.Stat(dupeFile); !os.IsNotExist(err) {
		t.Errorf("dupe file should have been moved out of src, still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dupes, "IMG_0042.jpg")); err != nil {
		t.Errorf("dupe file should have landed in the dupes dir: %v", err)
	}
}

// TestPrecheckFolder_DryRunMovesNothing mirrors `gpsync duplicates resolve
// --dry-run`: same detection, no filesystem changes.
func TestPrecheckFolder_DryRunMovesNothing(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	db := openInfoTestDB(t)
	src := t.TempDir()
	dupes := t.TempDir()

	dupeFile := filepath.Join(src, "a.jpg")
	writeTestFileContent(t, dupeFile, "dupe-content")
	sum, err := hashing.SHA256File(dupeFile)
	if err != nil {
		t.Fatal(err)
	}
	mustNoErr(t, db.EnsurePending(sum, 12, "image/jpeg", filepath.Join(t.TempDir(), "a.jpg"), nil))

	var out bytes.Buffer
	res, err := precheckFolder(db, src, dupes, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.dupeFiles != 1 {
		t.Fatalf("got dupeFiles=%d, want 1", res.dupeFiles)
	}
	if _, err := os.Stat(dupeFile); err != nil {
		t.Errorf("--dry-run must not move anything: %v", err)
	}
}

// TestPrecheckFolder_LeavesTheCanonicalCopyAlone is the safety guard: if
// src is pointed at (or overlaps) the real library, a file whose ledger
// row's own source path IS this file must never be moved -- that would
// delete the actual backed-up original, not clear out a redundant copy.
func TestPrecheckFolder_LeavesTheCanonicalCopyAlone(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	db := openInfoTestDB(t)
	src := t.TempDir()
	dupes := t.TempDir()

	canonical := filepath.Join(src, "IMG_0042.jpg")
	writeTestFileContent(t, canonical, "the-actual-photo-bytes")
	sum, err := hashing.SHA256File(canonical)
	if err != nil {
		t.Fatal(err)
	}
	mustNoErr(t, db.EnsurePending(sum, 21, "image/jpeg", canonical, nil))
	mustNoErr(t, db.MarkUploaded(sum, "AGj1xyz", ""))

	var out bytes.Buffer
	res, err := precheckFolder(db, src, dupes, false, &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.dupeFiles != 0 || res.newFiles != 0 {
		t.Fatalf("the canonical copy must be neither counted as new nor as a dupe, got new=%d dupe=%d",
			res.newFiles, res.dupeFiles)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Errorf("the canonical copy must never be moved: %v", err)
	}
}
