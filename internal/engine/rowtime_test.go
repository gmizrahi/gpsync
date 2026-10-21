package engine

import (
	"database/sql"
	"testing"

	"github.com/gmizrahi/gpsync/internal/statedb"
)

func nullAt(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }

// Issue #52. The Uploaded tab reads uploaded_at -- the authoritative upload
// time, and the only one known for rows uploaded before status_changed_at
// existed. Every other tab reads when the row entered its status.
func TestRowTime_PicksTheRightTimeForTheTab(t *testing.T) {
	cases := []struct {
		name   string
		row    statedb.Upload
		want   float64
		wantOK bool
	}{
		{"uploaded reads uploaded_at, not status_changed_at",
			statedb.Upload{Status: "uploaded", UploadedAt: nullAt(200), StatusChangedAt: nullAt(999)}, 200, true},
		{"uploaded before status_changed_at existed is still known",
			statedb.Upload{Status: "uploaded", UploadedAt: nullAt(200)}, 200, true},
		{"pending reads status_changed_at",
			statedb.Upload{Status: "pending", StatusChangedAt: nullAt(300)}, 300, true},
		{"failed reads status_changed_at, never uploaded_at",
			statedb.Upload{Status: "failed_retryable", UploadedAt: nullAt(5), StatusChangedAt: nullAt(400)}, 400, true},
		{"a row from before the column is unknown, not 1970",
			statedb.Upload{Status: "ignored"}, 0, false},
	}
	for _, c := range cases {
		got, ok := RowTime(c.row)
		if ok != c.wantOK || (ok && got != c.want) {
			t.Errorf("%s: RowTime = %v, %v; want %v, %v", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

// A row with no known time sorts last in BOTH directions -- like
// Captured, and unlike a zero value, which would pass for the oldest.
func TestSortRows_When_UnknownSortsLastBothWays(t *testing.T) {
	rows := func() []statedb.Upload {
		return []statedb.Upload{
			{FirstSourcePath: "/b", Status: "pending", StatusChangedAt: nullAt(20)},
			{FirstSourcePath: "/unknown", Status: "pending"},
			{FirstSourcePath: "/a", Status: "pending", StatusChangedAt: nullAt(10)},
			{FirstSourcePath: "/c", Status: "pending", StatusChangedAt: nullAt(30)},
		}
	}
	order := func(rs []statedb.Upload) string {
		s := ""
		for _, r := range rs {
			s += r.FirstSourcePath + " "
		}
		return s
	}

	asc := rows()
	sortRows(asc, SortKeyWhen, false)
	if got, want := order(asc), "/a /b /c /unknown "; got != want {
		t.Errorf("ascending: %q, want %q", got, want)
	}
	desc := rows()
	sortRows(desc, SortKeyWhen, true)
	if got, want := order(desc), "/c /b /a /unknown "; got != want {
		t.Errorf("descending: %q, want %q", got, want)
	}
}
