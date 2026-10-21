package statedb

import (
	"sort"
	"testing"
)

// Issue #53. The "Uploaded over time" chart counts the rows
// SuccessfulUploadTimestamps selects in SQL; a drill-down from one of its
// bars filters ListAll with Upload.RealUpload in Go. If the two ever pick
// different rows, a bar and the list it opens disagree. Every state a row
// can be in is seeded here, and the two selections must match exactly.
func TestRealUpload_SelectsExactlyWhatTheActivityChartCounts(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("pending", 1, "image/jpeg", "/lib/p.jpg", nil))
	must(t, db.EnsurePending("uploaded", 2, "image/jpeg", "/lib/u.jpg", nil))
	must(t, db.MarkUploaded("uploaded", "media-1", ""))
	must(t, db.EnsureMarkedSynced("marked", 4, "image/jpeg", "/lib/m.jpg", nil))
	must(t, db.EnsurePending("failed", 8, "image/jpeg", "/lib/f.jpg", nil))
	must(t, db.MarkFailed("failed", true, "unsupported", "nope"))
	// The odd ones a migration or a hand edit could leave: a media ID with
	// no time, and a time with no media ID on a non-uploaded row.
	must(t, db.EnsurePending("id-no-time", 16, "image/jpeg", "/lib/a.jpg", nil))
	if _, err := db.conn.Exec(`UPDATE uploads SET status='uploaded', google_media_item_id='media-2', uploaded_at=NULL WHERE sha256='id-no-time'`); err != nil {
		t.Fatal(err)
	}
	must(t, db.EnsurePending("time-no-id", 32, "image/jpeg", "/lib/b.jpg", nil))
	if _, err := db.conn.Exec(`UPDATE uploads SET uploaded_at=1000 WHERE sha256='time-no-id'`); err != nil {
		t.Fatal(err)
	}

	stamps, err := db.SuccessfulUploadTimestamps()
	if err != nil {
		t.Fatal(err)
	}
	var fromSQL []int64
	for _, s := range stamps {
		fromSQL = append(fromSQL, s.Size)
	}
	all, err := db.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("ListAll returned %d rows, want all 6 regardless of status", len(all))
	}
	var fromGo []int64
	for _, u := range all {
		if u.RealUpload() {
			fromGo = append(fromGo, u.Size)
		}
	}
	sort.Slice(fromSQL, func(i, j int) bool { return fromSQL[i] < fromSQL[j] })
	sort.Slice(fromGo, func(i, j int) bool { return fromGo[i] < fromGo[j] })
	if len(fromSQL) != len(fromGo) {
		t.Fatalf("SQL selects sizes %v, RealUpload selects %v", fromSQL, fromGo)
	}
	for i := range fromSQL {
		if fromSQL[i] != fromGo[i] {
			t.Fatalf("SQL selects sizes %v, RealUpload selects %v", fromSQL, fromGo)
		}
	}
}
