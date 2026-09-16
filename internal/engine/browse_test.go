package engine

import "testing"

// TestBrowseFiles_ReturnsMatchingBucket proves each FileListKind reads the
// right ledger bucket -- pending, failed_retryable, failed_permanent are
// kept genuinely distinct even though failed_retryable and
// failed_permanent both come out of the same underlying ListFailures call.
func TestBrowseFiles_ReturnsMatchingBucket(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h-p1", 10, "image/jpeg", "/lib/2024/beach.jpg", nil))
	mustNoErr(t, db.EnsurePending("h-p2", 10, "image/jpeg", "/lib/2024/party.jpg", nil))
	mustNoErr(t, db.EnsurePending("h-r1", 10, "image/jpeg", "/lib/2024/retry.jpg", nil))
	mustNoErr(t, db.MarkFailed("h-r1", false, "THROTTLE", "held for retry"))
	mustNoErr(t, db.EnsurePending("h-f1", 10, "image/jpeg", "/lib/2024/bad.bmp", nil))
	mustNoErr(t, db.MarkFailed("h-f1", true, "UNSUPPORTED_EXTENSION", "'.bmp' is unsupported"))

	pending, err := BrowseFiles(db, BrowseQuery{Kind: FileListPending, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if pending.Total != 2 {
		t.Errorf("pending.Total = %d, want 2", pending.Total)
	}

	retryable, err := BrowseFiles(db, BrowseQuery{Kind: FileListFailedRetryable, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if retryable.Total != 1 || retryable.Rows[0].SHA256 != "h-r1" {
		t.Errorf("retryable = %+v, want exactly h-r1", retryable)
	}

	permanent, err := BrowseFiles(db, BrowseQuery{Kind: FileListFailedPermanent, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if permanent.Total != 1 || permanent.Rows[0].SHA256 != "h-f1" {
		t.Errorf("permanent = %+v, want exactly h-f1", permanent)
	}
}

// TestBrowseFiles_UploadedNeedsReviewIgnored_ReturnRespectiveBuckets proves
// the three kinds added to close the Browse gap identified by whatis'
// parity work (before this, an uploaded/needs_review/ignored row had no way
// to be SEEN in the dashboard -- only counted in a Statistics total) each
// read the right ledger bucket, backed by ListByStatus rather than
// ListPending/ListFailures.
func TestBrowseFiles_UploadedNeedsReviewIgnored_ReturnRespectiveBuckets(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h-up", 10, "image/jpeg", "/lib/up.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h-up", "media-1", ""))
	mustNoErr(t, db.EnsureNeedsReview("h-nr", 10, "image/jpeg", "/originals/nr.jpg", nil))
	mustNoErr(t, db.EnsureNeedsReview("h-ig", 10, "image/jpeg", "/originals/ig.jpg", nil))
	mustNoErr(t, db.ResolveNeedsReview("h-ig", false))

	uploaded, err := BrowseFiles(db, BrowseQuery{Kind: FileListUploaded, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.Total != 1 || uploaded.Rows[0].SHA256 != "h-up" {
		t.Errorf("uploaded = %+v, want exactly h-up", uploaded)
	}

	needsReview, err := BrowseFiles(db, BrowseQuery{Kind: FileListNeedsReview, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if needsReview.Total != 1 || needsReview.Rows[0].SHA256 != "h-nr" {
		t.Errorf("needsReview = %+v, want exactly h-nr", needsReview)
	}

	ignored, err := BrowseFiles(db, BrowseQuery{Kind: FileListIgnored, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if ignored.Total != 1 || ignored.Rows[0].SHA256 != "h-ig" {
		t.Errorf("ignored = %+v, want exactly h-ig", ignored)
	}
}

// TestBrowseFiles_SearchMatchesHash proves Search also matches a content
// hash, not just a path substring -- the part of whatis' parity that lets a
// pasted sha256 find its row without needing the local file's bytes to
// re-hash it.
func TestBrowseFiles_SearchMatchesHash(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("abc123def", 10, "image/jpeg", "/lib/one.jpg", nil))
	mustNoErr(t, db.EnsurePending("zzz999", 10, "image/jpeg", "/lib/two.jpg", nil))

	got, err := BrowseFiles(db, BrowseQuery{Kind: FileListPending, Search: "ABC123", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 || got.Rows[0].SHA256 != "abc123def" {
		t.Errorf("got = %+v, want exactly abc123def (case-insensitive hash match)", got)
	}
}

// TestBrowseFiles_SearchFiltersByPathSubstring_CaseInsensitive proves the
// search box narrows results rather than listing everything.
func TestBrowseFiles_SearchFiltersByPathSubstring_CaseInsensitive(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h-1", 10, "image/jpeg", "/lib/2024/Beach Trip/img1.jpg", nil))
	mustNoErr(t, db.EnsurePending("h-2", 10, "image/jpeg", "/lib/2024/Winter/img2.jpg", nil))

	got, err := BrowseFiles(db, BrowseQuery{Kind: FileListPending, Search: "beach", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 1 || got.Rows[0].SHA256 != "h-1" {
		t.Errorf("got = %+v, want exactly h-1 (case-insensitive match on 'beach')", got)
	}
}

// TestBrowseFiles_Pagination proves offset/limit actually page through
// results and Total reflects the full matching count, not just this page.
func TestBrowseFiles_Pagination(t *testing.T) {
	db := openTestDB(t)
	paths := []string{"/lib/a.jpg", "/lib/b.jpg", "/lib/c.jpg", "/lib/d.jpg", "/lib/e.jpg"}
	for i, p := range paths {
		mustNoErr(t, db.EnsurePending(string(rune('1'+i)), 10, "image/jpeg", p, nil))
	}

	page1, err := BrowseFiles(db, BrowseQuery{Kind: FileListPending, Offset: 0, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Rows) != 2 || page1.Total != 5 || page1.Offset != 0 {
		t.Fatalf("page1 = %+v, want 2 rows, Total=5, Offset=0", page1)
	}

	page2, err := BrowseFiles(db, BrowseQuery{Kind: FileListPending, Offset: 2, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Rows) != 2 || page2.Total != 5 || page2.Offset != 2 {
		t.Fatalf("page2 = %+v, want 2 rows, Total=5, Offset=2", page2)
	}

	lastPage, err := BrowseFiles(db, BrowseQuery{Kind: FileListPending, Offset: 4, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(lastPage.Rows) != 1 {
		t.Fatalf("lastPage rows = %d, want 1 (only one left past offset 4)", len(lastPage.Rows))
	}

	pastEnd, err := BrowseFiles(db, BrowseQuery{Kind: FileListPending, Offset: 100, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(pastEnd.Rows) != 0 {
		t.Errorf("pastEnd rows = %d, want 0 (offset past total must not panic or wrap)", len(pastEnd.Rows))
	}
}

// TestBrowseFiles_UnknownKind_Errors guards against a typo'd query
// parameter silently returning an empty (rather than clearly wrong) list.
func TestBrowseFiles_UnknownKind_Errors(t *testing.T) {
	db := openTestDB(t)
	if _, err := BrowseFiles(db, BrowseQuery{Kind: FileListKind("bogus"), Limit: 100}); err == nil {
		t.Error("BrowseFiles error = nil, want an error for an unknown kind")
	}
}

// TestBrowseFiles_SortBySize proves numeric sort works correctly (not
// lexicographic -- "9" must sort before "10").
func TestBrowseFiles_SortBySize(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h-big", 500, "image/jpeg", "/lib/big.jpg", nil))
	mustNoErr(t, db.EnsurePending("h-small", 10, "image/jpeg", "/lib/small.jpg", nil))
	mustNoErr(t, db.EnsurePending("h-mid", 100, "image/jpeg", "/lib/mid.jpg", nil))

	asc, err := BrowseFiles(db, BrowseQuery{Kind: FileListPending, Sort: SortKeySize, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	wantAsc := []string{"h-small", "h-mid", "h-big"}
	for i, w := range wantAsc {
		if asc.Rows[i].SHA256 != w {
			t.Fatalf("asc[%d] = %s, want %s (full: %+v)", i, asc.Rows[i].SHA256, w, asc.Rows)
		}
	}

	desc, err := BrowseFiles(db, BrowseQuery{Kind: FileListPending, Sort: SortKeySize, SortDesc: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	wantDesc := []string{"h-big", "h-mid", "h-small"}
	for i, w := range wantDesc {
		if desc.Rows[i].SHA256 != w {
			t.Fatalf("desc[%d] = %s, want %s (full: %+v)", i, desc.Rows[i].SHA256, w, desc.Rows)
		}
	}
}

// TestBrowseFiles_SortByName sorts by basename, not full path -- two files
// in different folders whose full paths would sort one way must sort by
// filename alone here.
func TestBrowseFiles_SortByName(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsurePending("h-1", 10, "image/jpeg", "/lib/zzz-folder/a.jpg", nil))
	mustNoErr(t, db.EnsurePending("h-2", 10, "image/jpeg", "/lib/aaa-folder/z.jpg", nil))

	got, err := BrowseFiles(db, BrowseQuery{Kind: FileListPending, Sort: SortKeyName, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	// a.jpg (in zzz-folder) sorts before z.jpg (in aaa-folder) by NAME,
	// even though the reverse is true by full path.
	if got.Rows[0].SHA256 != "h-1" || got.Rows[1].SHA256 != "h-2" {
		t.Errorf("got = %+v, want h-1 (a.jpg) before h-2 (z.jpg)", got.Rows)
	}
}

// TestBrowseFiles_SortByCaptured_UnknownDatesSortLast proves a row with no
// known capture date never misleadingly sorts as "oldest" (1970) on either
// sort direction.
func TestBrowseFiles_SortByCaptured_UnknownDatesSortLast(t *testing.T) {
	db := openTestDB(t)
	known := 1700000000.0
	mustNoErr(t, db.EnsurePending("h-known", 10, "image/jpeg", "/lib/known.jpg", &known))
	mustNoErr(t, db.EnsurePending("h-unknown", 10, "image/jpeg", "/lib/unknown.jpg", nil))

	asc, err := BrowseFiles(db, BrowseQuery{Kind: FileListPending, Sort: SortKeyCaptured, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if asc.Rows[0].SHA256 != "h-known" || asc.Rows[1].SHA256 != "h-unknown" {
		t.Errorf("asc = %+v, want known first, unknown last", asc.Rows)
	}

	desc, err := BrowseFiles(db, BrowseQuery{Kind: FileListPending, Sort: SortKeyCaptured, SortDesc: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if desc.Rows[0].SHA256 != "h-known" || desc.Rows[1].SHA256 != "h-unknown" {
		t.Errorf("desc = %+v, want unknown still last even sorted descending", desc.Rows)
	}
}
