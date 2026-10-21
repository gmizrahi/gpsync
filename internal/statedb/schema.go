package statedb

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const schema = `
CREATE TABLE IF NOT EXISTS uploads (
    sha256 TEXT PRIMARY KEY,
    size INTEGER,
    mime_type TEXT,
    status TEXT NOT NULL,
    google_media_item_id TEXT,
    first_source_path TEXT,
    captured_at REAL,
    uploaded_at REAL,
    last_error_code TEXT,
    last_error_message TEXT,
    attempt_count INTEGER NOT NULL DEFAULT 0,
    -- status_changed_at is when the row last entered its status -- or, for
    -- a failure, last failed again, since a repeat failure keeps the status
    -- and bumps attempt_count. Browse shows it as Queued / Last failed /
    -- Ignored / Flagged. It is maintained by the triggers in
    -- statusChangedTriggers, never by hand: fourteen statements change a
    -- row's status, and a timestamp each of them had to remember to set
    -- would be one forgotten call site away from silently wrong. NULL for
    -- rows from before it existed, which Browse shows as unknown.
    status_changed_at REAL,
    -- missing_since / missing_checked_at / missing_confirmed_at track a file
    -- that is no longer at first_source_path. A scan FLAGS it (missing_since,
    -- kept from the first sighting; missing_checked_at, refreshed every time
    -- a scan finds it still absent). Only a FULL scan of every source folder
    -- can CONFIRM it (missing_confirmed_at), and only if the same content was
    -- not found at another path -- a moved or renamed file is repointed
    -- instead. Nothing is ever deleted from here by a scan; forgetting a
    -- confirmed-missing file is an explicit gpsync recheck --missing.
    missing_since REAL,
    missing_checked_at REAL,
    missing_confirmed_at REAL,
    -- verified_at/verify_error record the last time this item was
    -- confirmed to still EXIST in Google Photos (an exact mediaItems.get
    -- by the stored id). Until this existed, "uploaded" meant only that
    -- an API call once returned 200 -- a backup tool that had never
    -- verified a backup. NULL means never checked.
    verified_at REAL,
    verify_error TEXT,
    -- remote_quality records what the copy in Google Photos is BELIEVED
    -- to be: 'original', 'storage_saver', or NULL for unknown. The API
    -- exposes no storage-tier field, so this can only ever come from the
    -- person who put the file there -- see mark-synced's --quality flag.
    --
    -- It exists because inferring quality was a real mistake: a first
    -- version of gpsync reupload treated every mark-synced row (no
    -- google_media_item_id) as suspect, which would have re-sent 15,276
    -- files / 251 GiB that were already originals. "gpsync did not
    -- upload this" and "this is low quality" are different facts.
    remote_quality TEXT
);
CREATE INDEX IF NOT EXISTS idx_uploads_status ON uploads(status);
-- Folder-prefix lookups (first_source_path LIKE 'C:\\Photos\\2024\\%') run for
-- every folder a scan sweeps. NOCASE matches LIKE's own case-insensitivity,
-- which is what lets SQLite turn the prefix into an index range instead of
-- reading every row.
CREATE INDEX IF NOT EXISTS idx_uploads_path ON uploads(first_source_path COLLATE NOCASE);

-- path is COLLATE NOCASE: Windows filesystems are case-insensitive (but
-- case-preserving), and gpsync only ever runs there -- "C:\Photos\x.mp4" and
-- "C:\photos\x.mp4" are the same file, not two. Without this, scanning the
-- same folder under different casing on different occasions (easy to do by
-- accident on Windows) created a second files_seen row for the identical
-- file, which then showed up as a false "duplicate" -- and worse, as
-- reclaimable disk space -- in 'gpsync duplicates'. See migrateFilesSeenCaseInsensitive
-- for how an existing state.sqlite predating this gets fixed in place.
CREATE TABLE IF NOT EXISTS files_seen (
    path TEXT PRIMARY KEY COLLATE NOCASE,
    sha256 TEXT,
    mtime REAL,
    size INTEGER,
    last_scanned REAL
);
-- "does any path still hold this content" (see SupersededRows), asked once
-- per uploads row. Without this it re-reads all of files_seen each time,
-- which on a real ledger of tens of thousands of rows is quadratic -- the
-- same shape as the two scan-per-row queries already fixed in this file's
-- history.
CREATE INDEX IF NOT EXISTS idx_files_seen_sha256 ON files_seen(sha256);

CREATE TABLE IF NOT EXISTS quota_daily (
    date_pt TEXT PRIMARY KEY,
    requests_used INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS extension_stats (
    ext TEXT PRIMARY KEY,
    attempted INTEGER NOT NULL DEFAULT 0,
    succeeded INTEGER NOT NULL DEFAULT 0,
    rejected INTEGER NOT NULL DEFAULT 0,
    last_error TEXT
);

-- end_reason/end_detail record HOW the last run ended, not just that it
-- stopped: a run that gave up on a throttle, gave up on the daily quota, or
-- was killed with Ctrl+C all used to leave exactly the same trace as one
-- that finished (or, for Ctrl+C, a row stuck at status='running' forever).
-- Both are nullable and empty means "unset" -- see RunEnd/EndReason*.
CREATE TABLE IF NOT EXISTS run_progress (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    run_type TEXT,
    status TEXT,
    folders_total INTEGER,
    folders_done INTEGER,
    files_total INTEGER,
    files_done INTEGER,
    started_at REAL,
    updated_at REAL,
    end_reason TEXT,
    end_detail TEXT
);

-- throttle_events is a permanent, append-only record of every REAL
-- "concurrent write request" throttle the circuit breaker has ever hit
-- (deliberately NOT the separate daily-quota case, a completely different,
-- documented, once-a-day phenomenon that would contaminate this data).
-- It exists to answer how long recovery actually takes, and therefore
-- what the real backoff window should be. Recovery time and bytes
-- pushed between events are deliberately NOT stored here -- they're
-- derived at report time from uploads.uploaded_at (already recorded on
-- every successful upload), so this table only ever needs one INSERT per
-- throttle, never an UPDATE. See RecordThrottleEvent/ThrottleEvents/
-- ThrottleWindows.
CREATE TABLE IF NOT EXISTS throttle_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at REAL NOT NULL,
    rung INTEGER NOT NULL,
    wait_seconds REAL NOT NULL,
    message TEXT NOT NULL
);

-- dashboard_sessions backs the tray dashboard's login page, which
-- replaced HTTP Basic Auth. A
-- session token is opaque, unguessable (32 bytes of crypto/rand), and
-- looked up by exact match -- expires_at is enforced by the app, not
-- SQLite, since neither "remember me" nor a plain session both map to a
-- fixed TTL: a remembered login gets a long expiry, a plain one a short
-- one, but either way this table is the single source of truth for
-- whether a session cookie is still good, independent of the cookie's own
-- client-side Max-Age.
CREATE TABLE IF NOT EXISTS dashboard_sessions (
    token TEXT PRIMARY KEY,
    created_at REAL NOT NULL,
    expires_at REAL NOT NULL
);

-- crash_log backs "silent-death visibility": two real bugs this session
-- (a hand-edited config.toml's watch_heartbeat_minutes=0 panicking
-- time.NewTicker, and a WaitGroup misuse panic from a Start/Stop race,
-- both since fixed at the root cause) killed gpsync-tray's -H=windowsgui
-- process with NOTHING reaching tray.log or anywhere else visible. A
-- recovered panic is logged here so the NEXT launch's Status page can
-- show "last crash", not just a log file nobody thinks to go looking at.
CREATE TABLE IF NOT EXISTS crash_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    at REAL NOT NULL,
    context TEXT NOT NULL,
    message TEXT NOT NULL
);
`

// migrateFilesSeenCaseInsensitive rebuilds files_seen with a case-insensitive
// path collation if it was created by an older gpsync version without one (see
// the COLLATE NOCASE comment on the files_seen schema above for why this
// matters). Idempotent and cheap to check: no-ops immediately once already
// migrated, or on a brand-new database where the schema below creates the
// table correctly from the start. Rows that collide once case-folded (i.e.
// were the same physical file, recorded twice under different-case paths)
// are merged, keeping whichever was scanned most recently -- last_scanned
// is the tie-breaker, since it reflects the freshest known hash/mtime/size.
func migrateFilesSeenCaseInsensitive(conn *sql.DB) error {
	var createSQL string
	err := conn.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='files_seen'`).Scan(&createSQL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if strings.Contains(createSQL, "COLLATE NOCASE") {
		return nil
	}

	tx, err := conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		CREATE TABLE files_seen_migrated (
		    path TEXT PRIMARY KEY COLLATE NOCASE,
		    sha256 TEXT,
		    mtime REAL,
		    size INTEGER,
		    last_scanned REAL
		)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		INSERT INTO files_seen_migrated (path, sha256, mtime, size, last_scanned)
		SELECT path, sha256, mtime, size, last_scanned FROM files_seen
		ORDER BY last_scanned ASC
		ON CONFLICT(path) DO UPDATE SET
		    sha256=excluded.sha256, mtime=excluded.mtime,
		    size=excluded.size, last_scanned=excluded.last_scanned`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE files_seen`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE files_seen_migrated RENAME TO files_seen`); err != nil {
		return err
	}
	return tx.Commit()
}

// migrateUploadsVerified adds verified_at/verify_error to an uploads table
// created by an older gpsync version. Same idempotent PRAGMA-then-ALTER
// shape as the two migrations below; existing rows read back NULL, which
// correctly means "never verified".
func migrateUploadsVerified(conn *sql.DB) error {
	rows, err := conn.Query(`PRAGMA table_info(uploads)`)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for rows.Next() {
		var (
			cid        int
			name, ctyp string
			notNull    int
			dflt       sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &ctyp, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(existing) == 0 {
		return nil
	}
	for col, ddl := range map[string]string{
		"verified_at":          `ALTER TABLE uploads ADD COLUMN verified_at REAL`,
		"verify_error":         `ALTER TABLE uploads ADD COLUMN verify_error TEXT`,
		"remote_quality":       `ALTER TABLE uploads ADD COLUMN remote_quality TEXT`,
		"missing_since":        `ALTER TABLE uploads ADD COLUMN missing_since REAL`,
		"missing_checked_at":   `ALTER TABLE uploads ADD COLUMN missing_checked_at REAL`,
		"missing_confirmed_at": `ALTER TABLE uploads ADD COLUMN missing_confirmed_at REAL`,
		"status_changed_at":    `ALTER TABLE uploads ADD COLUMN status_changed_at REAL`,
	} {
		if existing[col] {
			continue
		}
		if _, err := conn.Exec(ddl); err != nil {
			return err
		}
	}
	return nil
}

// migrateDropAlbumTables removes the albums / upload_albums /
// pending_album_links tables. gpsync no longer creates albums at all, so
// these hold nothing anything reads.
//
// Why the feature went: album linking never worked against the live API
// for this app. `albums.batchAddMediaItems` rejected media item ids that
// `mediaItems.batchCreate` had minted seconds earlier ("Request contains
// an invalid media item id"), and albums this same client had just
// created came back 404 "does not match any albums" on the very next
// call. Verified on a real library: 16,305 outstanding links against 115
// that ever succeeded, with 2,291 of those failures belonging to uploads
// from the preceding 24 hours -- so it was not a legacy backlog, it was
// every attempt. The cause was never established, which is precisely why
// keeping the code would have meant keeping a feature nobody could fix.
//
// DROP, not DELETE: an empty table that nothing writes is a standing
// invitation to wonder whether it should be repopulated. Dropping is safe
// here because nothing reads these any more -- the ledger's own record of
// what was uploaded lives in `uploads`, and it is untouched.
func migrateDropAlbumTables(conn *sql.DB) error {
	for _, t := range []string{"pending_album_links", "upload_albums", "albums"} {
		if _, err := conn.Exec("DROP TABLE IF EXISTS " + t); err != nil {
			return fmt.Errorf("dropping %s: %w", t, err)
		}
	}
	return nil
}

// migrateRunProgressEndReason adds end_reason/end_detail to a run_progress
// table created by an older gpsync version. Idempotent and cheap: it asks
// SQLite what columns exist and adds only what's missing, so it no-ops on
// every run after the first (and on a brand-new database, where the schema
// constant already created them).
//
// ALTER TABLE ADD COLUMN rather than a table rebuild: these are new
// nullable columns with no constraints, so existing rows simply read back
// NULL -- which EndReason() maps to "unset", exactly right for a run that
// predates this tracking.
func migrateRunProgressEndReason(conn *sql.DB) error {
	rows, err := conn.Query(`PRAGMA table_info(run_progress)`)
	if err != nil {
		return err
	}
	existing := map[string]bool{}
	for rows.Next() {
		var (
			cid        int
			name, ctyp string
			notNull    int
			dflt       sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &ctyp, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	if len(existing) == 0 {
		return nil // no such table yet; the schema constant will create it
	}
	for _, col := range []string{"end_reason", "end_detail"} {
		if existing[col] {
			continue
		}
		if _, err := conn.Exec(`ALTER TABLE run_progress ADD COLUMN ` + col + ` TEXT`); err != nil {
			return err
		}
	}
	return nil
}

// MigrateToNeedsReview flips an EXISTING pending/failed_retryable row to
// needs_review -- for `gpsync clean-originals`, which backfills the
// originals-folder review state onto rows a scan queued BEFORE this
// feature existed. Deliberately a plain UPDATE, not EnsureNeedsReview:
// that one is an INSERT ... ON CONFLICT DO NOTHING, a no-op for a hash
// that's already tracked (which every row this method targets always is).
// Scoped to status IN ('pending','failed_retryable') so it can never touch
// an already-uploaded/already-resolved row, even given a stale caller.
func (db *DB) MigrateToNeedsReview(sha256 string) error {
	_, err := db.conn.Exec(
		`UPDATE uploads SET status='needs_review' WHERE sha256=? AND status IN ('pending', 'failed_retryable')`,
		sha256,
	)
	return err
}

// statusChangedTriggers keeps uploads.status_changed_at current: stamped
// when a row is inserted, and again whenever its status changes or its
// attempt_count does. MarkFailed is the only statement that bumps
// attempt_count, and it does so on every failure, so a file failing again
// in the same status counts -- "Last failed" means the latest failure, not
// the first. The two resets to 0 always change the status too.
//
// Applied by Open AFTER migrateUploadsVerified, not as part of schema.
// SQLite would accept it earlier -- it resolves a trigger's body when the
// trigger fires, not when it is created, so creating one that names a
// column the table does not have yet succeeds (measured). But if the
// migration then failed, the ledger would hold a trigger with no column,
// and every status change would fail with "no such column". Installing it
// after the migration means the trigger never exists without its column.
//
// The trigger's own UPDATE writes only status_changed_at, which is not in
// the UPDATE OF list, so it cannot re-fire itself.
//
// unixepoch('subsec') is REAL seconds since the epoch, the same unit as
// every other timestamp in this table (statedb.now()). It needs SQLite
// 3.42+; modernc.org/sqlite carries 3.53.
const statusChangedTriggers = `
CREATE TRIGGER IF NOT EXISTS uploads_status_changed_on_insert
AFTER INSERT ON uploads
BEGIN
    UPDATE uploads SET status_changed_at = unixepoch('subsec') WHERE sha256 = NEW.sha256;
END;

CREATE TRIGGER IF NOT EXISTS uploads_status_changed_on_update
AFTER UPDATE OF status, attempt_count ON uploads
WHEN NEW.status IS NOT OLD.status OR NEW.attempt_count IS NOT OLD.attempt_count
BEGIN
    UPDATE uploads SET status_changed_at = unixepoch('subsec') WHERE sha256 = NEW.sha256;
END;
`
