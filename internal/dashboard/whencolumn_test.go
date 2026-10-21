package dashboard

import (
	"strings"
	"testing"

	"github.com/gmizrahi/gpsync/internal/engine"
)

// Issue #52. One timestamp column, headed by what it means on each tab.
func TestBrowse_WhenColumn_HeadedPerTab(t *testing.T) {
	db := openTestDB(t)
	for kind, label := range browseWhenLabels {
		page, err := renderBrowsePage(db, kind, "", "", false, 0, engine.BrowseFilter{}, "", false, nil, "", "")
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if !strings.Contains(page, `sort=when`) || !strings.Contains(page, ">"+label+"</a>") {
			t.Errorf("%s: no sortable %q column header", kind, label)
		}
	}
	for _, k := range []engine.FileListKind{engine.FileListPending, engine.FileListFailedRetryable, engine.FileListFailedPermanent,
		engine.FileListUploaded, engine.FileListNeedsReview, engine.FileListIgnored} {
		if browseWhenLabels[k] == "" {
			t.Errorf("tab %s has no timestamp-column heading", k)
		}
	}
}

// Mark as already synced records status 'uploaded' AND an uploaded_at --
// the moment of marking -- with no media item ID. Shown bare, that reads as
// an upload that never happened, so it is tagged; a real upload is not.
func TestBrowse_MarkedSyncedRow_IsTaggedAndARealUploadIsNot(t *testing.T) {
	db := openTestDB(t)
	mustNoErr(t, db.EnsureMarkedSynced("h-marked", 1, "image/jpeg", "/lib/marked.jpg", nil))
	mustNoErr(t, db.EnsurePending("h-real", 1, "image/jpeg", "/lib/real.jpg", nil))
	mustNoErr(t, db.MarkUploaded("h-real", "media-1", ""))

	page, err := renderBrowsePage(db, engine.FileListUploaded, "", "", false, 0, engine.BrowseFilter{}, "", false, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(page, ">marked synced</span>"); n != 1 {
		t.Errorf("%d rows tagged marked synced, want exactly 1 (the marked one, not the real upload)", n)
	}
	row := page[strings.Index(page, "marked.jpg"):]
	row = row[:strings.Index(row, "</tr>")]
	if !strings.Contains(row, "marked synced") {
		t.Error("the tag is on the wrong row")
	}
}
