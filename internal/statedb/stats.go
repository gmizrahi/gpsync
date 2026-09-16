package statedb

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
)

// ResetStatusUnder resets every upload row reachable (via files_seen) from
// the given path prefixes back to 'pending', regardless of current status
// (uploaded, mark-synced, or failed) -- the data behind `gpsync sync --force`,
// for a folder the user wants re-pushed no matter what the ledger currently
// says about it. Returns the number of rows reset.
func (db *DB) ResetStatusUnder(prefixes []string) (int64, error) {
	if len(prefixes) == 0 {
		return 0, fmt.Errorf("ResetStatusUnder requires at least one path prefix (refusing to reset the whole library)")
	}
	where := "("
	args := []any{}
	for i, p := range prefixes {
		if i > 0 {
			where += " OR "
		}
		where += `path LIKE ? ESCAPE '\'`
		args = append(args, folderLikePattern(p))
	}
	where += ")"

	res, err := db.conn.Exec(
		`UPDATE uploads SET status='pending', google_media_item_id=NULL, uploaded_at=NULL,
		 last_error_code=NULL, last_error_message=NULL, attempt_count=0
		 WHERE sha256 IN (SELECT DISTINCT sha256 FROM files_seen WHERE `+where+`)`,
		args...,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountUploads returns the number of ledger entries.
func (db *DB) CountUploads() (int, error) {
	var n int
	err := db.conn.QueryRow(`SELECT COUNT(*) FROM uploads`).Scan(&n)
	return n, err
}

// StatRow is one row of the minimal per-file data the Statistics page
// aggregates by extension/media kind/capture year -- deliberately just
// the three columns that computation actually needs, not the full
// uploadCols row shape ListPending/ListPendingUnder fetch.
type StatRow struct {
	Path       string
	Size       int64
	CapturedAt sql.NullFloat64 // unix seconds; NULL for files with no read­able capture date
	// Status is the ledger status, so a caller can split totals by what
	// is still outstanding -- the Statistics page's "By media type" card
	// shows pending against total, and a whole-library figure cannot be
	// filtered down to that after the fact.
	Status string
}

// AllSizes returns every tracked hash's path/size/capture-date,
// REGARDLESS of status (pending, uploaded, failed -- everything the
// ledger has ever hashed) -- the Statistics page describes the whole
// library, not just what is left to do.
func (db *DB) AllSizes() ([]StatRow, error) {
	rows, err := db.conn.Query(`SELECT first_source_path, size, captured_at, status FROM uploads`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatRow
	for rows.Next() {
		var r StatRow
		if err := rows.Scan(&r.Path, &r.Size, &r.CapturedAt, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UploadedSince reports how many files (and how many total bytes) were
// actually uploaded to Google at or after sinceEpoch (Unix seconds) -- the
// "uploaded today" counter, paired with quota.PacificMidnightEpoch() by the
// caller so "today" means the same reset boundary the daily API quota
// already uses. Deliberately excludes `gpsync mark-synced` rows
// (google_media_item_id IS NULL): those never moved any bytes, so counting
// them here would overstate real upload throughput -- the same
// UploadedByGPB/MarkedSynced distinction LedgerSummary already draws.
func (db *DB) UploadedSince(sinceEpoch float64) (count int, totalBytes int64, err error) {
	err = db.conn.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(size), 0) FROM uploads
		 WHERE status='uploaded' AND google_media_item_id IS NOT NULL AND uploaded_at >= ?`,
		sinceEpoch,
	).Scan(&count, &totalBytes)
	return count, totalBytes, err
}

func (db *DB) CountsByStatus() (map[string]int, error) {
	rows, err := db.conn.Query(`SELECT status, COUNT(*) FROM uploads GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// LedgerSummary is the full picture behind `gpsync status` -- every distinct
// hash the ledger knows about, split by how it got there (a real upload via
// gpsync vs a `gpsync mark-synced` snapshot -- both are stored as status='uploaded'
// since they mean the same thing to `gpsync upload` -- "don't touch this
// again" -- but are worth reporting separately since they're very different
// operations: one moved bytes, one didn't), plus ledger-wide totals.
type LedgerSummary struct {
	Pending         int
	UploadedByGPB   int // status='uploaded' with a real google_media_item_id
	MarkedSynced    int // status='uploaded' via `gpsync mark-synced` -- no media item id, since nothing was actually sent
	FailedRetryable int
	FailedPermanent int
	// NeedsReview/Ignored: originals-folder review state (see
	// scanner.IsInOriginalsFolder) -- without a dedicated bucket here,
	// these hashes were counted in TotalHashes/TotalBytesTracked but
	// invisible in every OTHER printed line, so a reader couldn't tell
	// where they'd gone.
	NeedsReview       int
	NeedsReviewBytes  int64
	Ignored           int
	TotalHashes       int   // distinct content hashes tracked (rows in `uploads`)
	TotalBytesTracked int64 // sum of size across all tracked hashes
	TotalFilesSeen    int   // rows in `files_seen` -- can exceed TotalHashes when duplicate content exists under multiple paths
	TotalFolders      int   // distinct parent folders across files_seen
	// PendingBytes/SyncedBytes split TotalBytesTracked by status, for a
	// real request: "total size in library (synced, pending, total)".
	// Free -- the byte-per-status-group figure is already computed in the
	// query below, just not kept until now.
	PendingBytes int64
	SyncedBytes  int64 // UploadedByGPB + MarkedSynced combined, same grouping Synced uses everywhere else
	// FailedRetryableBytes is remaining work too, not a failure total:
	// RequeueRetryable (unscoped, run at the top of every Uploader.Run)
	// sweeps these rows straight back to 'pending', so anything
	// projecting how much is LEFT to upload has to count them alongside
	// PendingBytes or it under-reports. Free for the same reason
	// PendingBytes was -- the per-status byte sum is already computed by
	// the query below and was simply being discarded for this bucket.
	FailedRetryableBytes int64
}

func (db *DB) LedgerSummary() (LedgerSummary, error) {
	var s LedgerSummary

	rows, err := db.conn.Query(`SELECT status, google_media_item_id, COUNT(*), COALESCE(SUM(size), 0) FROM uploads GROUP BY status, google_media_item_id IS NULL`)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var status string
		var mediaItemID sql.NullString
		var n int
		var bytes int64
		if err := rows.Scan(&status, &mediaItemID, &n, &bytes); err != nil {
			rows.Close()
			return s, err
		}
		s.TotalBytesTracked += bytes
		s.TotalHashes += n
		switch {
		case status == "pending":
			s.Pending += n
			s.PendingBytes += bytes
		case status == "uploaded" && mediaItemID.Valid:
			s.UploadedByGPB += n
			s.SyncedBytes += bytes
		case status == "uploaded":
			s.MarkedSynced += n
			s.SyncedBytes += bytes
		case status == "failed_retryable":
			s.FailedRetryable += n
			s.FailedRetryableBytes += bytes
		case status == "failed_permanent":
			s.FailedPermanent += n
		case status == "needs_review":
			s.NeedsReview += n
			s.NeedsReviewBytes += bytes
		case status == "ignored":
			s.Ignored += n
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return s, err
	}
	rows.Close()

	if err := db.conn.QueryRow(`SELECT COUNT(*) FROM files_seen`).Scan(&s.TotalFilesSeen); err != nil {
		return s, err
	}

	pathRows, err := db.conn.Query(`SELECT path FROM files_seen`)
	if err != nil {
		return s, err
	}
	defer pathRows.Close()
	folders := map[string]bool{}
	for pathRows.Next() {
		var p string
		if err := pathRows.Scan(&p); err != nil {
			return s, err
		}
		folders[filepath.Dir(p)] = true
	}
	s.TotalFolders = len(folders)

	return s, pathRows.Err()
}

// ListByStatus returns every row with the exact given status, in
// first_source_path order -- matches ListPending/ListFailures' own default
// order. Unlike ListFailures (hard-scoped to the two failure statuses),
// this takes an arbitrary status so BrowseFiles can reach 'uploaded',
// 'needs_review', and 'ignored' rows without a dedicated method per status.
func (db *DB) ListByStatus(status string) ([]Upload, error) {
	rows, err := db.conn.Query(`SELECT `+uploadCols+` FROM uploads WHERE status=? ORDER BY first_source_path`, status)
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

// ── quota ───────────────────────────────────────────────────────────────

func (db *DB) ExtRecord(ext string, attempted, succeeded, rejected int, lastError string) error {
	var le sql.NullString
	if lastError != "" {
		le = sql.NullString{String: lastError, Valid: true}
	}
	_, err := db.conn.Exec(
		`INSERT INTO extension_stats (ext, attempted, succeeded, rejected, last_error) VALUES (?,?,?,?,?)
		 ON CONFLICT(ext) DO UPDATE SET
		   attempted=attempted+excluded.attempted,
		   succeeded=succeeded+excluded.succeeded,
		   rejected=rejected+excluded.rejected,
		   last_error=COALESCE(excluded.last_error, extension_stats.last_error)`,
		ext, attempted, succeeded, rejected, le,
	)
	return err
}

type ExtStat struct {
	Ext       string
	Attempted int
	Succeeded int
	Rejected  int
	LastError sql.NullString
}

func (db *DB) ExtStats() ([]ExtStat, error) {
	rows, err := db.conn.Query(`SELECT ext, attempted, succeeded, rejected, last_error FROM extension_stats ORDER BY ext`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExtStat
	for rows.Next() {
		var e ExtStat
		if err := rows.Scan(&e.Ext, &e.Attempted, &e.Succeeded, &e.Rejected, &e.LastError); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ── run progress (live "X of Y" view) ─────────────────────────────────

// RemoteQualityCounts breaks the uploaded set down by recorded quality,
// with "" meaning never recorded.
func (db *DB) RemoteQualityCounts() (map[string]int, error) {
	rows, err := db.conn.Query(
		`SELECT COALESCE(remote_quality,''), COUNT(*) FROM uploads WHERE status='uploaded' GROUP BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

// SyncStatusUnder reports, for files_seen rows under any of the given path
// prefixes, the total count and a breakdown of their upload status -- the
// data behind `gpsync sync`'s "X of Y fully synced" summary. Empty prefixes
// means the whole library.
func (db *DB) SyncStatusUnder(prefixes []string) (total int, byStatus map[string]int, err error) {
	byStatus = map[string]int{}

	where := ""
	args := []any{}
	if len(prefixes) > 0 {
		where = " WHERE ("
		for i, p := range prefixes {
			if i > 0 {
				where += " OR "
			}
			where += `f.path LIKE ? ESCAPE '\'`
			args = append(args, folderLikePattern(p))
		}
		where += ")"
	}

	totalRow := db.conn.QueryRow(`SELECT COUNT(*) FROM files_seen f`+where, args...)
	if err = totalRow.Scan(&total); err != nil {
		return 0, nil, err
	}

	rows, err := db.conn.Query(
		`SELECT u.status, COUNT(*) FROM files_seen f JOIN uploads u ON f.sha256=u.sha256`+where+` GROUP BY u.status`,
		args...,
	)
	if err != nil {
		return total, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return total, nil, err
		}
		byStatus[status] = n
	}
	return total, byStatus, rows.Err()
}

// SyncFileStatus is one row of the `gpsync sync --verbose` per-file listing.
type SyncFileStatus struct {
	Path    string
	Status  string
	Message string // last_error_message, if any
}

// SyncDetailUnder lists per-file status for files_seen rows under the given
// path prefixes -- the data behind `gpsync sync --verbose`'s per-file listing
// (e.g. "already uploaded" on a second run over the same folder).
func (db *DB) SyncDetailUnder(prefixes []string) ([]SyncFileStatus, error) {
	where := ""
	args := []any{}
	if len(prefixes) > 0 {
		where = " WHERE ("
		for i, p := range prefixes {
			if i > 0 {
				where += " OR "
			}
			where += `f.path LIKE ? ESCAPE '\'`
			args = append(args, folderLikePattern(p))
		}
		where += ")"
	}

	rows, err := db.conn.Query(
		`SELECT f.path, u.status, u.last_error_message FROM files_seen f JOIN uploads u ON f.sha256=u.sha256`+
			where+` ORDER BY f.path`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SyncFileStatus
	for rows.Next() {
		var s SyncFileStatus
		var msg sql.NullString
		if err := rows.Scan(&s.Path, &s.Status, &msg); err != nil {
			return nil, err
		}
		s.Message = msg.String
		out = append(out, s)
	}
	return out, rows.Err()
}

// DuplicateGroup is one set of file paths that all hash to the same
// content -- candidates for manual disk cleanup (only one copy needs to
// stay locally; the ledger already treats them as a single backed-up item).
type DuplicateGroup struct {
	SHA256 string
	Size   int64
	Paths  []string
}

// DuplicateGroups returns every content hash tracked under more than one
// path, largest reclaimable space first (Size * (len(Paths)-1) -- what
// you'd free by keeping just one copy of each group).
func (db *DB) DuplicateGroups() ([]DuplicateGroup, error) {
	rows, err := db.conn.Query(`
		SELECT f.sha256, u.size, f.path
		FROM files_seen f
		JOIN uploads u ON f.sha256 = u.sha256
		WHERE f.sha256 IN (SELECT sha256 FROM files_seen GROUP BY sha256 HAVING COUNT(*) > 1)
		ORDER BY f.sha256, f.path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	groupsBySHA := map[string]*DuplicateGroup{}
	var order []string
	for rows.Next() {
		var sha256, path string
		var size int64
		if err := rows.Scan(&sha256, &size, &path); err != nil {
			return nil, err
		}
		g, ok := groupsBySHA[sha256]
		if !ok {
			g = &DuplicateGroup{SHA256: sha256, Size: size}
			groupsBySHA[sha256] = g
			order = append(order, sha256)
		}
		g.Paths = append(g.Paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]DuplicateGroup, 0, len(order))
	for _, sha256 := range order {
		out = append(out, *groupsBySHA[sha256])
	}
	sort.Slice(out, func(i, j int) bool {
		reclaimI := out[i].Size * int64(len(out[i].Paths)-1)
		reclaimJ := out[j].Size * int64(len(out[j].Paths)-1)
		return reclaimI > reclaimJ
	})
	return out, nil
}
