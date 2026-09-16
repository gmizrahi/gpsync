package statedb

import (
	"database/sql"
	"path/filepath"
)

// EnsureNeedsReview inserts as 'needs_review' if this hash hasn't been seen
// before -- for a file discovered inside an "originals" folder (see
// scanner.IsInOriginalsFolder), which must never be auto-queued: it's
// usually a pre-edit backup copy of a file with the SAME NAME but different
// bytes (and therefore a different hash) already tracked elsewhere, and
// auto-uploading it produced pure duplicate noise in Google Photos.
// Same ON CONFLICT DO NOTHING semantics as
// EnsurePending: only applies to a genuinely first-seen hash, never
// downgrades an already-tracked pending/uploaded/failed row (which could
// happen if the exact same bytes were already seen at some OTHER,
// non-originals path first).
func (db *DB) EnsureNeedsReview(sha256 string, size int64, mimeType, sourcePath string, capturedAt *float64) error {
	var ca sql.NullFloat64
	if capturedAt != nil {
		ca = sql.NullFloat64{Float64: *capturedAt, Valid: true}
	}
	_, err := db.conn.Exec(
		`INSERT INTO uploads (sha256, size, mime_type, status, first_source_path, captured_at, attempt_count)
		 VALUES (?,?,?, 'needs_review', ?, ?, 0)
		 ON CONFLICT(sha256) DO NOTHING`,
		sha256, size, mimeType, sourcePath, ca,
	)
	return err
}

// ResolveNeedsReview applies the user's choice for one needs_review item --
// queue=true moves it to 'pending' (upload it normally), queue=false moves
// it to 'ignored' (permanent exclusion, never queued again). The
// WHERE status='needs_review' guard means a stale or replayed form
// submission can't resurrect an already-resolved row or flip an unrelated
// one -- the same defensive-scoping discipline as
// engine.FindDuplicateGroupAndValidateKeep.
func (db *DB) ResolveNeedsReview(sha256 string, queue bool) error {
	newStatus := "ignored"
	if queue {
		newStatus = "pending"
	}
	_, err := db.conn.Exec(
		`UPDATE uploads SET status=? WHERE sha256=? AND status='needs_review'`,
		newStatus, sha256,
	)
	return err
}

// NeedsReviewItem is one file awaiting the originals-folder review
// decision -- queue and upload, or add to the ignore list.
type NeedsReviewItem struct {
	SHA256          string
	Size            int64
	FirstSourcePath string
}

// NeedsReviewItems returns every current needs_review row, sorted by path
// for a stable, deterministic review order (matches DuplicateGroups' own
// sort-for-determinism precedent).
func (db *DB) NeedsReviewItems() ([]NeedsReviewItem, error) {
	rows, err := db.conn.Query(
		`SELECT sha256, size, first_source_path FROM uploads WHERE status='needs_review' ORDER BY first_source_path`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NeedsReviewItem
	for rows.Next() {
		var it NeedsReviewItem
		if err := rows.Scan(&it.SHA256, &it.Size, &it.FirstSourcePath); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// originalsFolderSep is filepath.Separator, indirected so a test can
// exercise the Windows-style ('\') pattern-building/escaping logic from a
// non-Windows test environment, where filepath.Separator is compile-time
// fixed to something else (this project's own tests run from WSL/Linux,
// where it's '/') and can never be made to behave like Windows otherwise.
// This is exactly the environment gap that let a real bug reach
// production undetected: see originalsLikePattern's own doc comment. Used
// by originalsLikePattern AND FindByBasename -- both had the identical
// bug independently (each built its own pattern by hand before this was
// factored out), so both share this one override point now.
var originalsFolderSep = string(filepath.Separator)

// originalsLikePattern builds the SQL LIKE pattern matching any path
// containing an "originals" path segment.
//
// A first version concatenated the raw, UNESCAPED separator directly into
// the pattern string -- on Windows, where the separator IS '\', that
// collided with the SAME character chosen as the LIKE ESCAPE character
// just below (ESCAPE '\'): the raw backslashes in the pattern got parsed
// as escape-introducers for whatever character followed them (e.g. "\o"
// -> a literal "o", silently dropping the backslash from what's actually
// matched) instead of literal backslashes to match against the path --
// so the pattern never matched ANY real Windows path at all. A real,
// reported bug: `gpsync originals-uploaded` returned nothing despite the
// user confirming a real match existed. Silent, not an error, and
// invisible to this project's own Linux-only test suite, since '/' (this
// environment's separator) doesn't collide with the escape character the
// same way. Fixed by running the WHOLE literal fragment (separator +
// "originals" + separator) through likeEscaper together, the same
// technique folderLikePattern already uses correctly elsewhere in this
// file -- on Linux this is a no-op (nothing needs escaping), and on
// Windows it correctly doubles each backslash (`\\`, a LITERAL single
// backslash under ESCAPE '\').
func originalsLikePattern() string {
	return "%" + likeEscaper.Replace(originalsFolderSep+"originals"+originalsFolderSep) + "%"
}

// OriginalsFolderRowsPendingOrFailed returns every hash that was queued as
// 'pending' or 'failed_retryable' by a scan from BEFORE the originals-folder
// review feature existed, but whose tracked path is inside an "originals"
// folder -- the backlog `gpsync clean-originals` migrates into needs_review.
// Matches on files_seen.path (not uploads.first_source_path) since a hash
// can have been seen at multiple paths; ANY tracked path being inside an
// originals folder is enough to flag it for review.
//
// The LIKE clause below is only a coarse, cheap pre-filter (SQLite's
// default LIKE is already case-insensitive for ASCII) -- this package
// can't import scanner.IsInOriginalsFolder for the exact check without an
// import cycle (scanner already imports statedb), so the caller
// (cmd/gpsync, which imports both) re-checks every returned path with that
// exact function before acting on it. Without this pre-filter, a
// multi-thousand-row pending/failed_retryable backlog would need
// returning and filtering in Go in full just to find a handful of real
// matches.
func (db *DB) OriginalsFolderRowsPendingOrFailed() ([]NeedsReviewItem, error) {
	pattern := originalsLikePattern()
	rows, err := db.conn.Query(
		`SELECT DISTINCT u.sha256, u.size, fs.path FROM uploads u
		 JOIN files_seen fs ON fs.sha256 = u.sha256
		 WHERE u.status IN ('pending', 'failed_retryable') AND fs.path LIKE ? ESCAPE '\'
		 ORDER BY fs.path`,
		pattern,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NeedsReviewItem
	for rows.Next() {
		var it NeedsReviewItem
		if err := rows.Scan(&it.SHA256, &it.Size, &it.FirstSourcePath); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// UploadedOriginalsRow is one file already sent to Google whose tracked
// path is inside an "originals" folder -- a candidate for a duplicate
// already sitting in Google Photos, from before this feature existed.
type UploadedOriginalsRow struct {
	SHA256          string
	Size            int64
	FirstSourcePath string
	MediaItemID     string // "" if this hash was uploaded via `gpsync mark-synced` (no real media item id)
}

// OriginalsFolderRowsUploaded returns every hash currently status='uploaded'
// whose tracked path is inside an "originals" folder: a way to find and
// manually clean up duplicates that were ALREADY pushed to Google Photos
// before the review workflow existed (so
// there's nothing left for gpsync itself to do about them; Google Photos has
// no API for finding/deleting a specific upload by path). Same coarse SQL
// pre-filter as OriginalsFolderRowsPendingOrFailed -- see its own doc
// comment for why the caller re-checks every candidate with the exact
// scanner.IsInOriginalsFolder.
func (db *DB) OriginalsFolderRowsUploaded() ([]UploadedOriginalsRow, error) {
	pattern := originalsLikePattern()
	rows, err := db.conn.Query(
		`SELECT DISTINCT u.sha256, u.size, fs.path, COALESCE(u.google_media_item_id, '') FROM uploads u
		 JOIN files_seen fs ON fs.sha256 = u.sha256
		 WHERE u.status = 'uploaded' AND fs.path LIKE ? ESCAPE '\'
		 ORDER BY fs.path`,
		pattern,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UploadedOriginalsRow
	for rows.Next() {
		var r UploadedOriginalsRow
		if err := rows.Scan(&r.SHA256, &r.Size, &r.FirstSourcePath, &r.MediaItemID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
