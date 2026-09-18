package statedb

import (
	"database/sql"
	"errors"
	"strings"
)

type Upload struct {
	SHA256            string
	Size              int64
	MimeType          string
	Status            string
	GoogleMediaItemID sql.NullString
	FirstSourcePath   string
	CapturedAt        sql.NullFloat64
	UploadedAt        sql.NullFloat64
	LastErrorCode     sql.NullString
	LastErrorMessage  sql.NullString
	AttemptCount      int
}

func (db *DB) GetUpload(sha256 string) (*Upload, error) {
	row := db.conn.QueryRow(`SELECT `+uploadCols+` FROM uploads WHERE sha256=?`, sha256)
	u, err := scanUpload(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// EnsurePending inserts as pending if this hash hasn't been seen before.
// Never downgrades an already-uploaded/failed row back to pending.
func (db *DB) EnsurePending(sha256 string, size int64, mimeType, sourcePath string, capturedAt *float64) error {
	var ca sql.NullFloat64
	if capturedAt != nil {
		ca = sql.NullFloat64{Float64: *capturedAt, Valid: true}
	}
	_, err := db.conn.Exec(
		`INSERT INTO uploads (sha256, size, mime_type, status, first_source_path, captured_at, attempt_count)
		 VALUES (?,?,?, 'pending', ?, ?, 0)
		 ON CONFLICT(sha256) DO NOTHING`,
		sha256, size, mimeType, sourcePath, ca,
	)
	return err
}

// EnsureMarkedSynced inserts as 'uploaded' (no media item ID -- there isn't
// one, since gpsync never actually uploaded it) if this hash hasn't been seen
// before. For content the user confirms is already backed up outside of
// gpsync (e.g. via the Google Photos app directly).
//
// A hash already known as 'pending' (e.g. scanned before mark-synced ran
// against it) is promoted to 'uploaded' -- that's the whole point of
// mark-synced, and 'pending' isn't a state worth protecting. A hash already
// 'uploaded' (either a real upload or a prior mark-synced) or in a failed
// state is left untouched -- mark-synced never downgrades a real failure
// into silently-ignored, and never needs to touch an already-synced hash.
func (db *DB) EnsureMarkedSynced(sha256 string, size int64, mimeType, sourcePath string, capturedAt *float64) error {
	var ca sql.NullFloat64
	if capturedAt != nil {
		ca = sql.NullFloat64{Float64: *capturedAt, Valid: true}
	}
	t := now()
	_, err := db.conn.Exec(
		`INSERT INTO uploads (sha256, size, mime_type, status, first_source_path, captured_at, uploaded_at, attempt_count)
		 VALUES (?,?,?, 'uploaded', ?, ?, ?, 0)
		 ON CONFLICT(sha256) DO UPDATE SET status='uploaded', uploaded_at=?
		 WHERE uploads.status='pending'`,
		sha256, size, mimeType, sourcePath, ca, t, t,
	)
	return err
}

// MarkUploaded records a successful upload. quality is what gpsync just
// SENT ("original" or "storage_saver"), which it knows for certain because
// it is the setting the upload was made under -- pass "" only where that
// is genuinely unknown.
//
// Recording it here closes a gap that kept reopening: remote_quality used
// to be set only by `mark-synced --quality` and `gpsync quality`, so every
// file gpsync uploaded itself landed with the field empty and had to be
// backfilled by hand afterwards. A real library had drifted to 6,524
// unrecorded rows that way -- all of them original, all of them known to
// be original at the moment they were written, and none of them saying so.
// `gpsync reupload` reads this field to decide what needs replacing, so an
// empty value is the difference between "confirmed fine" and "no idea".
func (db *DB) MarkUploaded(sha256, googleMediaItemID, quality string) error {
	if quality == "" {
		_, err := db.conn.Exec(
			`UPDATE uploads SET status='uploaded', google_media_item_id=?, uploaded_at=?,
			 last_error_code=NULL, last_error_message=NULL WHERE sha256=?`,
			googleMediaItemID, now(), sha256,
		)
		return err
	}
	_, err := db.conn.Exec(
		`UPDATE uploads SET status='uploaded', google_media_item_id=?, uploaded_at=?, remote_quality=?,
		 last_error_code=NULL, last_error_message=NULL WHERE sha256=?`,
		googleMediaItemID, now(), quality, sha256,
	)
	return err
}

func (db *DB) MarkFailed(sha256 string, permanent bool, errorCode, errorMessage string) error {
	status := "failed_retryable"
	if permanent {
		status = "failed_permanent"
	}
	_, err := db.conn.Exec(
		`UPDATE uploads SET status=?, last_error_code=?, last_error_message=?,
		 attempt_count=attempt_count+1 WHERE sha256=?`,
		status, errorCode, errorMessage, sha256,
	)
	return err
}

// DeleteUpload removes one hash's ledger row entirely, addressed by hash
// rather than by folder prefix -- the surgical counterpart to
// ResetStatusUnder, which can only work at folder scope.
//
// For `gpsync recheck` clearing a stale permanent failure whose source file no
// longer exists: there is nothing left to retry and nothing to protect. It
// is also low-risk to get wrong, because a permanent failure means nothing
// was ever accepted by Google for this hash -- so if identical content
// reappears at some new path later, the next scan's EnsurePending simply
// creates a fresh pending row. No duplicate upload is possible.
func (db *DB) DeleteUpload(sha256 string) error {
	_, err := db.conn.Exec(`DELETE FROM uploads WHERE sha256=?`, sha256)
	return err
}

// UpdateFirstSourcePath repoints a hash's ledger row at a different local
// copy of its content -- for `gpsync duplicates resolve`, whose whole job is
// to move OTHER copies of a hash away, potentially including whichever
// path first_source_path currently names. A row still 'pending' or
// 'failed_retryable' gets re-read from first_source_path on the next
// upload run; left stale, that read hits a file duplicates resolve just
// moved into the trash and fails permanently with "no longer exists on
// disk" for content that's sitting right there under a different path.
// Callers must confirm path is still real (os.Stat) before calling this --
// unconditional here on purpose, since the caller already did that check
// as its OWN precondition for proceeding with the move.
func (db *DB) UpdateFirstSourcePath(sha256, path string) error {
	_, err := db.conn.Exec(`UPDATE uploads SET first_source_path=? WHERE sha256=?`, path, sha256)
	return err
}

// ── missing files ──────────────────────────────────────────────────────
//
// A file deleted or moved on disk used to leave its ledger entry pointing
// at a path that no longer exists, forever: scans only ever look at files
// that ARE there. These methods back a two-step lifecycle -- a scan flags,
// a full scan confirms or repoints -- so a file that was merely moved to a
// folder the current scan did not cover is never mistaken for a deletion.

// UploadsUnder returns every ledger entry, of any status, whose recorded
// path is under one of prefixes.
func (db *DB) UploadsUnder(prefixes []string) ([]MissingSweepRow, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	where, args := likeAnyPrefix("first_source_path", prefixes)
	// #nosec G202 -- `where` is built by likeAnyPrefix from ? placeholders
	// only; every value travels in args. No user data is concatenated.
	rows, err := db.conn.Query(`SELECT sha256, first_source_path, missing_since IS NOT NULL FROM uploads WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissingSweepRow
	for rows.Next() {
		var r MissingSweepRow
		if err := rows.Scan(&r.SHA256, &r.Path, &r.Flagged); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResetFailedToPending puts a single failed hash back in the queue and
// clears the recorded error, so the next run treats it as new work.
//
// Deliberately scoped to one hash and guarded on the current status: unlike
// ResetStatusUnder (whole folder, any status) this cannot disturb a
// neighbouring file that is already uploaded, which is exactly the blast
// radius that made `gpsync sync --force` the wrong tool for fixing one stray
// record. Returns the number of rows changed, so a caller can tell a real
// reset from a no-op.
func (db *DB) ResetFailedToPending(sha256 string) (int64, error) {
	res, err := db.conn.Exec(
		`UPDATE uploads SET status='pending', last_error_code=NULL, last_error_message=NULL
		 WHERE sha256=? AND status IN ('failed_permanent','failed_retryable')`,
		sha256,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RequeueRetryable moves failed_retryable rows back to pending (e.g. new day / manual retry).
func (db *DB) RequeueRetryable() (int64, error) {
	res, err := db.conn.Exec(`UPDATE uploads SET status='pending' WHERE status='failed_retryable'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (db *DB) ListPending() ([]Upload, error) {
	rows, err := db.conn.Query(`SELECT ` + uploadCols + ` FROM uploads WHERE status='pending' ORDER BY first_source_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// ListPendingUnder returns pending rows whose source path falls under any
// of the given folder prefixes, so `gpsync upload`/`gpsync sync` can be
// scoped to just-scanned folder(s) instead of the entire library's backlog
// — essential for working through a very large library incrementally
// rather than waiting for a single whole-library pass.
func (db *DB) ListPendingUnder(prefixes []string) ([]Upload, error) {
	if len(prefixes) == 0 {
		return db.ListPending()
	}
	sql := `SELECT ` + uploadCols + ` FROM uploads WHERE status='pending' AND (`
	args := make([]any, 0, len(prefixes))
	for i, p := range prefixes {
		if i > 0 {
			sql += " OR "
		}
		sql += `first_source_path LIKE ? ESCAPE '\'`
		args = append(args, folderLikePattern(p))
	}
	sql += ") ORDER BY first_source_path"

	rows, err := db.conn.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// SumPendingSizeUnder returns the file count and total byte size of pending
// rows under the given folder prefixes -- the same scope ListPendingUnder
// uses, but a cheap aggregate instead of fetching every row, for a caller
// that only needs "how much work is this" (gpsync-tray's dashboard, wanting a
// CLI-parity "Transferred: X / Y bytes" line for the run it's about to
// start) rather than the rows themselves. An empty prefixes list matches
// ListPending's own "no scope" behavior: every pending row, library-wide.
func (db *DB) SumPendingSizeUnder(prefixes []string) (count int, totalBytes int64, err error) {
	sql := `SELECT COUNT(*), COALESCE(SUM(size), 0) FROM uploads WHERE status='pending'`
	var args []any
	if len(prefixes) > 0 {
		sql += " AND ("
		for i, p := range prefixes {
			if i > 0 {
				sql += " OR "
			}
			sql += `first_source_path LIKE ? ESCAPE '\'`
			args = append(args, folderLikePattern(p))
		}
		sql += ")"
	}
	err = db.conn.QueryRow(sql, args...).Scan(&count, &totalBytes)
	return count, totalBytes, err
}

// CaptureDateRow is one row of the minimal data `gpsync fix-dates` needs to
// reconsider a file's captured_at -- the hash to write an update against,
// the path to re-derive a date from, and the currently-stored value to
// compare against.
type CaptureDateRow struct {
	SHA256     string
	Path       string
	CapturedAt sql.NullFloat64
}

// AllCaptureDates returns every tracked hash's sha256/path/captured_at,
// regardless of status -- the fix a bad capture date needs applies
// library-wide, not just to pending files. Backs `gpsync fix-dates` (see
// UpdateCapturedAt).
func (db *DB) AllCaptureDates() ([]CaptureDateRow, error) {
	rows, err := db.conn.Query(`SELECT sha256, first_source_path, captured_at FROM uploads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CaptureDateRow
	for rows.Next() {
		var r CaptureDateRow
		if err := rows.Scan(&r.SHA256, &r.Path, &r.CapturedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateCapturedAt overwrites one row's captured_at -- used by `gpsync
// fix-dates` to replace a value derived from an unreliable filesystem
// mtime with one recovered from the file's own path. Nothing else about
// the row (status, size, etc.) is touched.
func (db *DB) UpdateCapturedAt(sha256 string, capturedAt float64) error {
	_, err := db.conn.Exec(`UPDATE uploads SET captured_at=? WHERE sha256=?`, capturedAt, sha256)
	return err
}

// LastUploadAt reports when the most recent real upload to Google
// completed, and whether there has ever been one. Same filter as
// UploadedSince (status='uploaded' AND google_media_item_id IS NOT NULL)
// and for the same reason: a `gpsync mark-synced` row carries an
// uploaded_at too, but it records a bookkeeping decision, not a transfer,
// so letting one satisfy "last successful upload" would report the
// pipeline as healthy at a moment when nothing had moved in days.
//
// Read from the ledger rather than from the tray's in-memory recent-event
// list so the answer survives a restart -- "when did this last actually
// work" is precisely the question you ask after a process came back up.
func (db *DB) LastUploadAt() (at float64, ok bool, err error) {
	var v sql.NullFloat64
	err = db.conn.QueryRow(
		`SELECT MAX(uploaded_at) FROM uploads
		 WHERE status='uploaded' AND google_media_item_id IS NOT NULL`,
	).Scan(&v)
	if err != nil {
		return 0, false, err
	}
	return v.Float64, v.Valid, nil
}

func (db *DB) ListFailures(permanentOnly bool) ([]Upload, error) {
	query := `SELECT ` + uploadCols + ` FROM uploads WHERE status IN ('failed_permanent','failed_retryable') ORDER BY first_source_path`
	if permanentOnly {
		query = `SELECT ` + uploadCols + ` FROM uploads WHERE status='failed_permanent' ORDER BY first_source_path`
	}
	rows, err := db.conn.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		u, err := scanUpload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// UploadedAtSize is one successful upload's timestamp and size -- the
// minimal data BuildThrottleAnalysis needs to compute every throttle
// event's clean-window activity and real recovery time.
type UploadedAtSize struct {
	At   float64
	Size int64
}

// MarkedSyncedUnder lists rows recorded as uploaded but with no
// google_media_item_id -- i.e. registered by `gpsync mark-synced` or an
// earlier rclone-era workflow, never actually sent BY gpsync. These are
// precisely the files whose remote quality is unknown, and so the ones a
// re-upload would upgrade. An empty prefixes list means the whole ledger.
func (db *DB) MarkedSyncedUnder(prefixes []string) ([]Upload, error) {
	q := `SELECT sha256, size, COALESCE(mime_type,''), status, COALESCE(first_source_path,'')
	        FROM uploads
	       WHERE status='uploaded' AND remote_quality=?`
	args0 := []any{QualityStorageSaver}
	args := args0
	if len(prefixes) > 0 {
		q += " AND ("
		for i, p := range prefixes {
			if i > 0 {
				q += " OR "
			}
			q += `first_source_path LIKE ? ESCAPE '\'`
			args = append(args, folderLikePattern(p))
		}
		q += ")"
	}
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Upload
	for rows.Next() {
		var u Upload
		if err := rows.Scan(&u.SHA256, &u.Size, &u.MimeType, &u.Status, &u.FirstSourcePath); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RequeueForReupload puts specific hashes back to 'pending' so the
// uploader sends the local original. Takes an explicit hash list rather
// than a path scope so the caller can exclude files that no longer exist
// on disk -- re-queueing one of those would only produce a permanent
// failure on the next run.
//
// Google merges a re-uploaded original over a lower-quality copy of the
// same photo in place rather than duplicating it; that observed behaviour
// is the entire reason this project refuses to reconcile against the API
// (see CLAUDE.md's "No reconcile-against-the-API step" rule), and it is
// what makes this command worth having.
// Quality values for uploads.remote_quality.
const (
	QualityOriginal     = "original"
	QualityStorageSaver = "storage_saver"
)

// QualityFor maps a config upload_quality value ("original" |
// "space_saver") to the remote_quality this ledger records
// ("original" | "storage_saver"). Two vocabularies for the same idea,
// and they are NOT interchangeable: config names the local downscale
// setting, this column names what is stored at Google. Anything
// unrecognised maps to "" -- better unrecorded than wrong, since nothing
// can detect a wrong verdict afterwards (the API exposes no storage tier).
func QualityFor(uploadQuality string) string {
	switch strings.ToLower(strings.TrimSpace(uploadQuality)) {
	case "original":
		return QualityOriginal
	case "space_saver", "storage_saver", "storage-saver":
		return QualityStorageSaver
	default:
		return ""
	}
}

// SetRemoteQuality records what the copy in Google Photos is believed to
// be, for files already recorded as uploaded. Scoped by path prefix, or
// the whole ledger when prefixes is empty.
func (db *DB) SetRemoteQuality(quality string, prefixes []string) (int64, error) {
	q := `UPDATE uploads SET remote_quality=? WHERE status='uploaded'`
	args := []any{quality}
	if len(prefixes) > 0 {
		q += " AND ("
		for i, p := range prefixes {
			if i > 0 {
				q += " OR "
			}
			q += `first_source_path LIKE ? ESCAPE '\'`
			args = append(args, folderLikePattern(p))
		}
		q += ")"
	}
	res, err := db.conn.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetRemoteQualityForPaths records quality for an exact set of files,
// rather than for everything under a folder prefix. Needed because
// `gpsync mark-synced` accepts individual files and file globs: passing
// one of those through folderLikePattern produces "...\*.mp4\%", which
// matches nothing, so --quality would silently record nothing for exactly
// the arguments the file support was added for.
//
// Chunked because SQLite caps host parameters per statement (999 by
// default) and a glob can easily name more files than that.
func (db *DB) SetRemoteQualityForPaths(quality string, paths []string) (int64, error) {
	const chunk = 400
	var total int64
	for start := 0; start < len(paths); start += chunk {
		end := start + chunk
		if end > len(paths) {
			end = len(paths)
		}
		batch := paths[start:end]
		q := `UPDATE uploads SET remote_quality=? WHERE status='uploaded' AND first_source_path IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",") + `)`
		args := []any{quality}
		for _, p := range batch {
			args = append(args, p)
		}
		res, err := db.conn.Exec(q, args...)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// UnmarkResult breaks a revert down by what it actually touched, because
// the two halves have very different costs.
type UnmarkResult struct {
	// Asserted is rows that were mark-synced: a claim that something else
	// already held the file. Reverting one costs nothing but a rescan.
	Asserted int64
	// Uploaded is rows gpsync really uploaded. Reverting one means those
	// bytes get sent again -- which is exactly how you replace a
	// storage-saver copy with an original, and also exactly how you
	// accidentally re-send a library.
	Uploaded int64
	// UploadedBytes is how much re-sending those will cost.
	UploadedBytes int64
}

// UnmarkSyncedUnder and UnmarkSyncedPaths undo a `gpsync mark-synced`,
// putting rows back to 'pending' so a normal upload picks them up, which
// is how a mistaken mark-synced is undone.
//
// They revert REAL gpsync uploads too, not just mark-synced assertions.
// An earlier version refused to, on the reasoning that a real upload is a
// receipt and re-sending is waste. That was too clever by half: Google
// merges a re-uploaded original over a storage-saver copy in place, so
// requeuing a file IS the supported way to upgrade its quality. The
// caller is told what the revert covers (see UnmarkResult) rather than
// being prevented from asking for it.
//
// remote_quality and the Google media item id are both cleared with the
// status: they described a copy this row no longer claims.
func (db *DB) UnmarkSyncedUnder(prefixes []string) (UnmarkResult, error) {
	if len(prefixes) == 0 {
		return UnmarkResult{}, nil
	}
	where := "("
	args := []any{}
	for i, p := range prefixes {
		if i > 0 {
			where += " OR "
		}
		where += `first_source_path LIKE ? ESCAPE '\'`
		args = append(args, folderLikePattern(p))
	}
	where += ")"
	return db.unmark(where, args)
}

func (db *DB) UnmarkSyncedPaths(paths []string) (UnmarkResult, error) {
	var out UnmarkResult
	const chunk = 400
	for start := 0; start < len(paths); start += chunk {
		end := start + chunk
		if end > len(paths) {
			end = len(paths)
		}
		batch := paths[start:end]
		where := `first_source_path IN (` + strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",") + `)`
		args := make([]any, 0, len(batch))
		for _, p := range batch {
			args = append(args, p)
		}
		got, err := db.unmark(where, args)
		if err != nil {
			return out, err
		}
		out.Asserted += got.Asserted
		out.Uploaded += got.Uploaded
		out.UploadedBytes += got.UploadedBytes
	}
	return out, nil
}

// unmark counts BEFORE it writes -- once the rows are pending, the
// google_media_item_id that distinguished a real upload from an assertion
// is gone, so the breakdown could not be reconstructed afterwards.
func (db *DB) unmark(where string, args []any) (UnmarkResult, error) {
	var out UnmarkResult
	err := db.conn.QueryRow(
		`SELECT
		   COALESCE(SUM(CASE WHEN google_media_item_id IS NULL THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN google_media_item_id IS NOT NULL THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN google_media_item_id IS NOT NULL THEN size ELSE 0 END), 0)
		 FROM uploads WHERE status='uploaded' AND `+where, args...,
	).Scan(&out.Asserted, &out.Uploaded, &out.UploadedBytes)
	if err != nil {
		return out, err
	}
	_, err = db.conn.Exec(
		`UPDATE uploads SET status='pending', uploaded_at=NULL, remote_quality=NULL,
		   google_media_item_id=NULL, attempt_count=0, last_error_code=NULL, last_error_message=NULL
		 WHERE status='uploaded' AND `+where, args...)
	return out, err
}

func (db *DB) RequeueForReupload(sha256s []string) (int64, error) {
	if len(sha256s) == 0 {
		return 0, nil
	}
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE uploads
	                            SET status='pending', last_error_code=NULL, last_error_message=NULL
	                          WHERE sha256=? AND status='uploaded' AND remote_quality=` + "'" + QualityStorageSaver + "'")
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	var n int64
	for _, sha := range sha256s {
		res, err := stmt.Exec(sha)
		if err != nil {
			return n, err
		}
		if c, _ := res.RowsAffected(); c > 0 {
			n += c
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// SuccessfulUploadTimestamps returns every successful upload's (status=
// 'uploaded' with a real google_media_item_id -- a `gpsync mark-synced` row
// never actually contended for the write quota, so it says nothing about
// recovery) uploaded_at/size, ascending by uploaded_at. Replaces what used
// to be two separate queries called ONCE PER throttle_events ROW
// (FirstUploadAfter/UploadsBetween, both unindexed full scans of uploads)
// -- with throttle_events an append-only, never-pruned log, that pattern
// got slower every time a new throttle fired, until the Statistics page
// took about ten seconds to load.
// BuildThrottleAnalysis now fetches this list exactly once and walks it
// alongside throttle_events in a single linear pass instead.
func (db *DB) SuccessfulUploadTimestamps() ([]UploadedAtSize, error) {
	rows, err := db.conn.Query(
		`SELECT uploaded_at, size FROM uploads
		 WHERE google_media_item_id IS NOT NULL AND uploaded_at IS NOT NULL
		 ORDER BY uploaded_at`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UploadedAtSize
	for rows.Next() {
		var u UploadedAtSize
		if err := rows.Scan(&u.At, &u.Size); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SupersededRow is an uploads row whose recorded path now holds different
// content. CurrentSHA256 is what the last scan found there.
type SupersededRow struct {
	SHA256          string
	Status          string
	Size            int64
	FirstSourcePath string
	CurrentSHA256   string
}

// SupersededRows finds ledger rows whose recorded path is now known to hold
// different content, and whose own content is at no scanned path at all.
//
// The case this exists for: a photo edited in place -- a crop, a rotate, an
// exposure fix -- keeps its path but changes its content and therefore its
// hash. files_seen is keyed by path, so the next scan overwrites that path's
// hash with the new one, while the OLD hash's uploads row is left behind
// still naming that path. Nothing path-based notices, because the path is
// still there.
//
// Two conditions, and both are load-bearing:
//
// The join requires files_seen to still hold that path with a DIFFERENT
// hash. That is the difference between "edited" and "deleted": a scan prunes
// files_seen for a file that is gone, so a deleted file has no row here and
// never matches. Deletion is CheckMissingSourceFile's business, and it
// deliberately never forgets a row on its own -- an unplugged drive must
// lose nothing.
//
// The NOT EXISTS requires this row's own content to be at no scanned path.
// A file that was merely MOVED keeps its hash under a new path, and a second
// copy elsewhere keeps it too. Either one means the content is still held,
// so the row still describes something real.
func (db *DB) SupersededRows() ([]SupersededRow, error) {
	rows, err := db.conn.Query(
		`SELECT u.sha256, u.status, u.size, u.first_source_path, fs.sha256
		   FROM uploads u
		   JOIN files_seen fs ON fs.path = u.first_source_path
		  WHERE fs.sha256 <> u.sha256
		    AND NOT EXISTS (SELECT 1 FROM files_seen f2 WHERE f2.sha256 = u.sha256)
		  ORDER BY u.first_source_path`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SupersededRow
	for rows.Next() {
		var r SupersededRow
		if err := rows.Scan(&r.SHA256, &r.Status, &r.Size, &r.FirstSourcePath, &r.CurrentSHA256); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
