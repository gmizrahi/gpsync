package main

import (
	"path/filepath"
	"testing"
)

// TestCleanOriginals_DryRun_ReportsWithoutWriting proves the default
// (commit=false) reports what would change but writes nothing; only
// --commit applies it.
func TestCleanOriginals_DryRun_ReportsWithoutWriting(t *testing.T) {
	db := openInfoTestDB(t)
	p := filepath.Join("D:", "Photos", "2013", "originals", "a.jpg")
	mustNoErr(t, db.EnsurePending("h-orig", 100, "image/jpeg", p, nil))
	mustNoErr(t, db.UpsertFileSeen(p, "h-orig", 0, 100))

	sum, err := cleanOriginals(db, false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.matched != 1 {
		t.Fatalf("matched = %d, want 1", sum.matched)
	}

	u, err := db.GetUpload("h-orig")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "pending" {
		t.Errorf("Status = %q, want still pending -- dry-run must write nothing", u.Status)
	}
}

// TestCleanOriginals_Commit_MigratesMatchedRows proves --commit actually
// applies the migration to needs_review.
func TestCleanOriginals_Commit_MigratesMatchedRows(t *testing.T) {
	db := openInfoTestDB(t)
	p := filepath.Join("D:", "Photos", "2013", "originals", "a.jpg")
	mustNoErr(t, db.EnsurePending("h-orig", 100, "image/jpeg", p, nil))
	mustNoErr(t, db.UpsertFileSeen(p, "h-orig", 0, 100))

	sum, err := cleanOriginals(db, true)
	if err != nil {
		t.Fatal(err)
	}
	if sum.matched != 1 {
		t.Fatalf("matched = %d, want 1", sum.matched)
	}

	u, err := db.GetUpload("h-orig")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "needs_review" {
		t.Errorf("Status = %q, want needs_review after --commit", u.Status)
	}
}

// TestCleanOriginals_PlainPendingFile_NeverMatched proves an ordinary
// pending file (not inside an originals folder) is left completely
// alone -- the whole point of scanner.IsInOriginalsFolder's exact
// re-check on top of the SQL pre-filter.
func TestCleanOriginals_PlainPendingFile_NeverMatched(t *testing.T) {
	db := openInfoTestDB(t)
	p := filepath.Join("D:", "Photos", "2013", "a.jpg")
	mustNoErr(t, db.EnsurePending("h-plain", 100, "image/jpeg", p, nil))
	mustNoErr(t, db.UpsertFileSeen(p, "h-plain", 0, 100))

	sum, err := cleanOriginals(db, true)
	if err != nil {
		t.Fatal(err)
	}
	if sum.matched != 0 {
		t.Errorf("matched = %d, want 0", sum.matched)
	}

	u, err := db.GetUpload("h-plain")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != "pending" {
		t.Errorf("Status = %q, want unchanged pending", u.Status)
	}
}
