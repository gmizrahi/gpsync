package statedb

import (
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// These never compare two "now" stamps taken milliseconds apart -- that
// would be a test of the clock. Instead each plants a sentinel the trigger
// cannot have written (1.0, the first second of 1970) and asserts the
// transition under test replaced it, or left it alone.

const sentinel = 1.0

func statusChangedAt(t *testing.T, db *DB, sha string) sql.NullFloat64 {
	t.Helper()
	u, err := db.GetUpload(sha)
	if err != nil || u == nil {
		t.Fatalf("GetUpload(%s) = %v, %v", sha, u, err)
	}
	return u.StatusChangedAt
}

// plant writes the sentinel directly. The UPDATE touches neither status
// nor attempt_count, so it does not fire the trigger itself.
func plant(t *testing.T, db *DB, sha string) {
	t.Helper()
	if _, err := db.conn.Exec(`UPDATE uploads SET status_changed_at=? WHERE sha256=?`, sentinel, sha); err != nil {
		t.Fatal(err)
	}
	if got := statusChangedAt(t, db, sha); !got.Valid || got.Float64 != sentinel {
		t.Fatalf("planting the sentinel fired the trigger: got %+v", got)
	}
}

// assertStampedNow fails unless v is a real timestamp from this test run,
// in the same unit (REAL unix seconds) as every other time in the table.
func assertStampedNow(t *testing.T, v sql.NullFloat64, what string) {
	t.Helper()
	if !v.Valid {
		t.Fatalf("%s: status_changed_at is NULL, want it stamped", what)
	}
	if d := math.Abs(v.Float64 - float64(time.Now().Unix())); d > 60 {
		t.Fatalf("%s: status_changed_at = %v, %vs from now -- not stamped by this change, or the wrong unit", what, v.Float64, d)
	}
}

func TestStatusChangedAt_StampedOnInsert(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h1", 1, "image/jpeg", "/lib/a.jpg", nil))
	assertStampedNow(t, statusChangedAt(t, db, "h1"), "a new pending row")
}

func TestStatusChangedAt_StampedOnStatusChange(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h1", 1, "image/jpeg", "/lib/a.jpg", nil))
	plant(t, db, "h1")
	must(t, db.MarkFailed("h1", true, "unsupported", "nope"))
	assertStampedNow(t, statusChangedAt(t, db, "h1"), "pending -> failed_permanent")
}

// "Last failed" means the latest failure. A file failing again keeps
// status failed_retryable -- only attempt_count moves -- so a trigger on
// status alone would freeze the time at the first failure.
func TestStatusChangedAt_RepeatFailureInTheSameStatusCounts(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h1", 1, "image/jpeg", "/lib/a.jpg", nil))
	must(t, db.MarkFailed("h1", false, "throttle", "429"))
	plant(t, db, "h1")
	must(t, db.MarkFailed("h1", false, "throttle", "429"))
	assertStampedNow(t, statusChangedAt(t, db, "h1"), "a second failure in the same status")
}

// Bookkeeping that is not a status change must not move the time, or
// "Ignored since" would read as whenever a scan last looked at the file.
func TestStatusChangedAt_UnrelatedUpdateLeavesItAlone(t *testing.T) {
	db := openTestDB(t)
	must(t, db.EnsurePending("h1", 1, "image/jpeg", "/lib/a.jpg", nil))
	plant(t, db, "h1")
	must(t, db.MarkMissing("h1", float64(time.Now().Unix())))
	if got := statusChangedAt(t, db, "h1"); !got.Valid || got.Float64 != sentinel {
		t.Errorf("status_changed_at = %+v after a missing-file flag, want the sentinel untouched", got)
	}
}

// A ledger from before the column existed: Open adds it, old rows read
// NULL (unknown, never a made-up date), and the triggers work on the
// migrated table from then on. Opened twice, since Open runs every time.
func TestStatusChangedAt_OldLedgerMigrates(t *testing.T) {
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
		INSERT INTO uploads (sha256, size, mime_type, status, first_source_path)
		VALUES ('old', 1, 'image/jpeg', 'failed_retryable', '/lib/old.jpg');`); err != nil {
		t.Fatal(err)
	}
	must(t, raw.Close())

	for i := 0; i < 2; i++ {
		db, err := Open()
		if err != nil {
			t.Fatalf("open #%d of a pre-migration ledger: %v", i+1, err)
		}
		if i == 0 {
			if got := statusChangedAt(t, db, "old"); got.Valid {
				t.Errorf("a row from before the column existed reads %v, want NULL -- there is no honest value for it", got.Float64)
			}
			must(t, db.MarkFailed("old", false, "throttle", "429"))
			assertStampedNow(t, statusChangedAt(t, db, "old"), "a failure on the migrated table")
		}
		must(t, db.Close())
	}
}
