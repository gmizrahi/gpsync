package engine

import (
	"path/filepath"
	"testing"
)

// TestBuildOriginalsReviewItem_AssemblesItemAndCandidates covers the data
// behind the originals-folder review UI: the review item alongside
// candidate matches found elsewhere in the library by filename, so there
// is visual confirmation before deciding whether to queue or ignore.
// libRoot is a host-shaped fixture root: FindByBasename builds its LIKE
// pattern from filepath.Separator, which cannot match a POSIX literal on
// Windows.
var libRoot = filepath.Join(string(filepath.Separator), "lib")

func TestBuildOriginalsReviewItem_AssemblesItemAndCandidates(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsureNeedsReview("h-original", 100, "image/jpeg", filepath.Join(libRoot, "2013", "originals", "IMG_1234.jpg"), nil))
	mustNoErr(t, db.UpsertFileSeen(filepath.Join(libRoot, "2013", "originals", "IMG_1234.jpg"), "h-original", 0, 100))
	mustNoErr(t, db.EnsurePending("h-edited", 90, "image/jpeg", filepath.Join(libRoot, "2013", "IMG_1234.jpg"), nil))
	mustNoErr(t, db.UpsertFileSeen(filepath.Join(libRoot, "2013", "IMG_1234.jpg"), "h-edited", 0, 90))

	item, ok, err := BuildOriginalsReviewItem(db, "h-original")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false, want true for a real needs_review hash")
	}
	if item.FirstSourcePath != filepath.Join(libRoot, "2013", "originals", "IMG_1234.jpg") {
		t.Errorf("FirstSourcePath = %q, want the originals-folder path", item.FirstSourcePath)
	}
	if len(item.Candidates) != 1 {
		// Fatal, not Error: the next assertion indexes Candidates[0], which
		// panics on an empty slice and takes the whole package down with it.
		t.Fatalf("Candidates = %+v, want exactly the edited sibling", item.Candidates)
	}
	if item.Candidates[0].Path != filepath.Join(string(filepath.Separator), "lib", "2013", "IMG_1234.jpg") {
		t.Errorf("Candidates[0].Path = %q, want the edited sibling", item.Candidates[0].Path)
	}
	if item.Candidates[0].Status != "pending" {
		t.Errorf("Candidates[0].Status = %q, want pending", item.Candidates[0].Status)
	}
}

// TestBuildOriginalsReviewItem_AlreadyResolved_ReturnsNotOK covers a stale
// page reload after the item was already resolved (by this request or a
// concurrent one) -- must report ok=false, not a stale/empty item.
func TestBuildOriginalsReviewItem_AlreadyResolved_ReturnsNotOK(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsureNeedsReview("h-original", 100, "image/jpeg", filepath.Join(libRoot, "originals", "a.jpg"), nil))
	mustNoErr(t, db.ResolveNeedsReview("h-original", false)) // already ignored

	_, ok, err := BuildOriginalsReviewItem(db, "h-original")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("ok = true, want false for an already-resolved hash")
	}
}

// TestBuildOriginalsReviewItem_UnknownHash_ReturnsNotOK covers a hash
// that was never a needs_review row at all.
func TestBuildOriginalsReviewItem_UnknownHash_ReturnsNotOK(t *testing.T) {
	db := openTestDB(t)
	_, ok, err := BuildOriginalsReviewItem(db, "never-existed")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("ok = true, want false for an unknown hash")
	}
}

// TestAutoResolveObviousOriginals_ImmediateParentSibling_AutoIgnored
// covers the structurally certain case: a file whose exact sibling
// (scanner.SiblingPath) is tracked needs no human review at all.
func TestAutoResolveObviousOriginals_ImmediateParentSibling_AutoIgnored(t *testing.T) {
	db := openTestDB(t)
	originalsPath := filepath.Join("D:", "Photos", "2013", "originals", "IMG_1234.jpg")
	siblingPath := filepath.Join("D:", "Photos", "2013", "IMG_1234.jpg")

	mustNoErr(t, db.EnsureNeedsReview("h-original", 100, "image/jpeg", originalsPath, nil))
	mustNoErr(t, db.UpsertFileSeen(originalsPath, "h-original", 0, 100))
	mustNoErr(t, db.EnsurePending("h-edited", 90, "image/jpeg", siblingPath, nil))
	mustNoErr(t, db.UpsertFileSeen(siblingPath, "h-edited", 0, 90))

	sum, err := AutoResolveObviousOriginals(db)
	if err != nil {
		t.Fatal(err)
	}
	if sum.IgnoredCount != 1 {
		t.Errorf("IgnoredCount = %d, want 1", sum.IgnoredCount)
	}
	if sum.IgnoredBytes != 100 {
		t.Errorf("IgnoredBytes = %d, want 100", sum.IgnoredBytes)
	}
	u, err := db.GetUpload("h-original")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "ignored" {
		t.Errorf("Status = %q, want ignored", u.Status)
	}
}

// TestAutoResolveObviousOriginals_NoMatchAnywhere_StaysForReview covers
// the case an EARLIER version of this function auto-queued, on the
// assumption that nothing suggested it was a duplicate. A file with no
// match can just as easily be something deliberately discarded into the
// originals folder as a genuinely new photo -- only a human can tell
// which, so it must stay in needs_review, not be auto-resolved.
func TestAutoResolveObviousOriginals_NoMatchAnywhere_StaysForReview(t *testing.T) {
	db := openTestDB(t)
	originalsPath := filepath.Join("D:", "Photos", "2013", "originals", "UNIQUE_9999.jpg")

	mustNoErr(t, db.EnsureNeedsReview("h-unique", 100, "image/jpeg", originalsPath, nil))
	mustNoErr(t, db.UpsertFileSeen(originalsPath, "h-unique", 0, 100))

	sum, err := AutoResolveObviousOriginals(db)
	if err != nil {
		t.Fatal(err)
	}
	if sum.IgnoredCount != 0 {
		t.Errorf("IgnoredCount = %d, want 0", sum.IgnoredCount)
	}
	u, err := db.GetUpload("h-unique")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "needs_review" {
		t.Errorf("Status = %q, want still needs_review -- a real, direct correction: \"if there is no match, let me also review, sometimes those were bad quality photos i removed\"", u.Status)
	}
}

// TestAutoResolveObviousOriginals_MatchElsewhereNotImmediateParent_StaysForReview
// is the genuinely ambiguous case: no immediate-parent sibling, but the
// SAME filename exists somewhere else entirely (not the deterministic
// Picasa-style location) -- per the same correction: "if it's not in its
// parent, but do exist someplace else in our photo library, then we need
// a resolution, since the match is not immediate." Must be left alone for
// a human.
func TestAutoResolveObviousOriginals_MatchElsewhereNotImmediateParent_StaysForReview(t *testing.T) {
	db := openTestDB(t)
	originalsPath := filepath.Join("D:", "Photos", "2013", "originals", "IMG_1234.jpg")
	// No sibling at C:\Photos\2013\IMG_1234.jpg -- but the same name
	// exists in a totally different folder.
	elsewherePath := filepath.Join("D:", "Photos", "2014", "IMG_1234.jpg")

	mustNoErr(t, db.EnsureNeedsReview("h-original", 100, "image/jpeg", originalsPath, nil))
	mustNoErr(t, db.UpsertFileSeen(originalsPath, "h-original", 0, 100))
	mustNoErr(t, db.EnsurePending("h-elsewhere", 90, "image/jpeg", elsewherePath, nil))
	mustNoErr(t, db.UpsertFileSeen(elsewherePath, "h-elsewhere", 0, 90))

	sum, err := AutoResolveObviousOriginals(db)
	if err != nil {
		t.Fatal(err)
	}
	if sum.IgnoredCount != 0 {
		t.Errorf("IgnoredCount = %d, want 0 -- this case must stay in needs_review", sum.IgnoredCount)
	}
	u, err := db.GetUpload("h-original")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "needs_review" {
		t.Errorf("Status = %q, want still needs_review", u.Status)
	}
}
