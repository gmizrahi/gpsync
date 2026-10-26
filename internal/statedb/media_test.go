package statedb

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// Issue #68: a ledger from before the metadata columns gains them on Open,
// its rows read as never read (so a backfill picks them up), and
// RecordMediaMetadata writes a row once only.
func TestMediaMetadata_OldLedgerMigratesAndRecordsOnce(t *testing.T) {
	t.Setenv("GPSYNC_STATE_DIR", t.TempDir())
	StateDir = getStateDir()
	StateDBPath = filepath.Join(StateDir, "state.sqlite")
	if err := os.MkdirAll(StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", StateDBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE uploads (
		    sha256 TEXT PRIMARY KEY, size INTEGER, mime_type TEXT, status TEXT NOT NULL,
		    google_media_item_id TEXT, first_source_path TEXT, captured_at REAL, uploaded_at REAL,
		    last_error_code TEXT, last_error_message TEXT, attempt_count INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO uploads (sha256, size, mime_type, status, first_source_path, captured_at)
		VALUES ('old', 1, 'image/jpeg', 'uploaded', '/lib/old.jpg', 100);`); err != nil {
		t.Fatal(err)
	}
	must(t, raw.Close())

	db, err := Open()
	if err != nil {
		t.Fatalf("opening a pre-metadata ledger: %v", err)
	}
	defer db.Close()
	missing, err := db.SHA256sWithoutMetadata()
	if err != nil || !missing["old"] {
		t.Fatalf("SHA256sWithoutMetadata = %v, %v; want the old row", missing, err)
	}

	lat, lon, w, h := 1.5, -2.5, 30, 40
	at := 50.0
	wrote, err := db.RecordMediaMetadata("old", MediaMetadata{CapturedAt: &at, Source: "exif", Offset: "+02:00", Lat: &lat, Lon: &lon, Width: &w, Height: &h}, 7)
	if err != nil || !wrote {
		t.Fatalf("first record: %v, %v", wrote, err)
	}
	other := 99.0
	if wrote, err := db.RecordMediaMetadata("old", MediaMetadata{CapturedAt: &other, Source: "path"}, 8); err != nil || wrote {
		t.Errorf("second record wrote (%v, %v); want each row once", wrote, err)
	}
	m, err := db.GetListableMedia("old")
	if err != nil || m == nil {
		t.Fatal(err)
	}
	if m.CapturedAt != 50 || m.CaptureSource.String != "exif" || m.CaptureOffset.String != "+02:00" || m.Lat.Float64 != 1.5 || m.Lon.Float64 != -2.5 || m.Width.Int64 != 30 || m.Height.Int64 != 40 {
		t.Errorf("row = %+v", m)
	}
	if missing, _ := db.SHA256sWithoutMetadata(); missing["old"] {
		t.Error("the row still reads as never read")
	}

	// nil and empty fields are stored as unknown, and nil keeps captured_at.
	must(t, db.EnsurePending("new", 1, "video/mp4", "/lib/v.mp4", &other))
	if _, err := db.RecordMediaMetadata("new", MediaMetadata{}, 9); err != nil {
		t.Fatal(err)
	}
	if m, _ := db.GetListableMedia("new"); m.CapturedAt != 99 || m.CaptureSource.Valid || m.CaptureOffset.Valid || m.Lat.Valid || m.Width.Valid {
		t.Errorf("empty metadata: %+v; want captured_at kept and the rest NULL", m)
	}
}
