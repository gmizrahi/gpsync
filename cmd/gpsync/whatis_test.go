package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fatih/color"

	"github.com/gmizrahi/gpsync/internal/hashing"
)

// TestWhatisFile_ResolvesContentBackToItsOriginalPath proves the lookup
// that makes an unidentifiable photo identifiable again.
//
// Dedup identity here is the content SHA-256, not the path, so a copy of an
// uploaded photo -- downloaded back out of Google Photos under a generated
// name, saved to a temp folder -- still hashes to the value the ledger
// recorded, and can be traced to the local file it came from.
func TestWhatisFile_ResolvesContentBackToItsOriginalPath(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	db := openInfoTestDB(t)
	dir := t.TempDir()

	// The original, as gpsync would have scanned and uploaded it.
	original := filepath.Join(dir, "2024", "IMG_0042.jpg")
	writeTestFileContent(t, original, "the-actual-photo-bytes")
	sum, err := hashing.SHA256File(original)
	if err != nil {
		t.Fatal(err)
	}
	mustNoErr(t, db.EnsurePending(sum, 21, "image/jpeg", original, nil))
	mustNoErr(t, db.MarkUploaded(sum, "AGj1epXYZ123", ""))
	mustNoErr(t, db.UpsertFileSeen(original, sum, 0, 21))

	// The same content, downloaded back out under a name that says nothing.
	downloaded := filepath.Join(dir, "downloads", "download-8f3a1c.jpg")
	writeTestFileContent(t, downloaded, "the-actual-photo-bytes")

	out := captureStdout(t, func() {
		if err := whatisFile(db, downloaded); err != nil {
			t.Fatal(err)
		}
	})

	for _, want := range []string{
		sum,            // the identity it was matched on
		"found",        // it resolved
		original,       // ...to the original local file
		"uploaded",     // and what gpsync did with it
		"AGj1epXYZ123", // the Google media item id
	} {
		if !strings.Contains(out, want) {
			t.Errorf("whatis output is missing %q:\n%s", want, out)
		}
	}
}

// TestWhatisFile_ReportsUnknownContentClearly: an unmatched file must say
// so plainly, and explain the most likely reason, rather than looking like
// a failure.
func TestWhatisFile_ReportsUnknownContentClearly(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	db := openInfoTestDB(t)
	dir := t.TempDir()

	known := filepath.Join(dir, "known.jpg")
	writeTestFileContent(t, known, "known-content")
	sum, err := hashing.SHA256File(known)
	if err != nil {
		t.Fatal(err)
	}
	mustNoErr(t, db.EnsurePending(sum, 13, "image/jpeg", known, nil))

	stranger := filepath.Join(dir, "stranger.jpg")
	writeTestFileContent(t, stranger, "totally-different-content")

	out := captureStdout(t, func() {
		if err := whatisFile(db, stranger); err != nil {
			t.Fatalf("an unknown file must not be an error: %v", err)
		}
	})
	if !strings.Contains(out, "not found") {
		t.Errorf("expected a clear not-found message:\n%s", out)
	}
	if strings.Contains(out, known) {
		t.Errorf("unknown content was matched to an unrelated ledger entry:\n%s", out)
	}
	// The most common real reason deserves a hint.
	if !strings.Contains(out, "re-encoded") {
		t.Errorf("expected the output to explain why a genuine copy might not match:\n%s", out)
	}
}

// TestWhatisFile_ListsOtherCopiesOnDisk: when the same content is tracked
// under several paths, all of them are worth showing -- that is precisely
// the "which local file is this?" question being asked.
func TestWhatisFile_ListsOtherCopiesOnDisk(t *testing.T) {
	orig := color.NoColor
	color.NoColor = true
	defer func() { color.NoColor = orig }()

	db := openInfoTestDB(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "2024", "IMG_1.jpg")
	b := filepath.Join(dir, "backup", "copy-of-IMG_1.jpg")
	writeTestFileContent(t, a, "same-bytes")
	writeTestFileContent(t, b, "same-bytes")
	sum, err := hashing.SHA256File(a)
	if err != nil {
		t.Fatal(err)
	}
	mustNoErr(t, db.EnsurePending(sum, 10, "image/jpeg", a, nil))
	mustNoErr(t, db.MarkUploaded(sum, "m1", ""))
	mustNoErr(t, db.UpsertFileSeen(a, sum, 0, 10))
	mustNoErr(t, db.UpsertFileSeen(b, sum, 0, 10))

	out := captureStdout(t, func() {
		if err := whatisFile(db, a); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, b) {
		t.Errorf("expected the other on-disk copy %s to be listed:\n%s", b, out)
	}
}

// TestWhatisFile_RejectsUnreadableInput: a missing file or a directory must
// fail with an actionable message, not a hash of nothing.
func TestWhatisFile_RejectsUnreadableInput(t *testing.T) {
	db := openInfoTestDB(t)
	dir := t.TempDir()

	if err := whatisFile(db, filepath.Join(dir, "nope.jpg")); err == nil {
		t.Error("expected an error for a missing file")
	}
	if err := whatisFile(db, dir); err == nil {
		t.Error("expected an error when handed a directory")
	} else if !strings.Contains(err.Error(), "directory") {
		t.Errorf("error = %v, want it to say the path is a directory", err)
	}
}

func writeTestFileContent(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
