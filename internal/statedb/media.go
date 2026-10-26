package statedb

import (
	"database/sql"
	"errors"
)

// MediaRow is one uploads row as the media API (issue #66) lists it.
type MediaRow struct {
	SHA256          string
	CapturedAt      float64
	MimeType        string
	FirstSourcePath string
}

// mediaListable is what the media API leaves out: files the user ignored,
// files waiting for a review decision, and files a full scan confirmed are
// gone. Lookups by hash apply the same rule, so a file left out of the list
// can't be fetched by its hash either.
const mediaListable = `status NOT IN ('ignored', 'needs_review') AND missing_confirmed_at IS NULL`

// ListMedia returns up to limit listable rows with from <= captured_at < to,
// ordered by (captured_at, sha256), starting after the keyset position
// (afterAt, afterSHA); afterSHA "" starts at the beginning. Rows with no
// captured_at are never listed.
func (db *DB) ListMedia(from, to, afterAt float64, afterSHA string, limit int) ([]MediaRow, error) {
	q := `SELECT sha256, captured_at, COALESCE(mime_type, ''), COALESCE(first_source_path, '') FROM uploads
		 WHERE captured_at >= ? AND captured_at < ? AND ` + mediaListable
	args := []any{from, to}
	if afterSHA != "" {
		q += ` AND (captured_at > ? OR (captured_at = ? AND sha256 > ?))`
		args = append(args, afterAt, afterAt, afterSHA)
	}
	q += ` ORDER BY captured_at, sha256 LIMIT ?`
	args = append(args, limit)
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MediaRow
	for rows.Next() {
		var m MediaRow
		if err := rows.Scan(&m.SHA256, &m.CapturedAt, &m.MimeType, &m.FirstSourcePath); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetListableMedia returns the row for sha256 if the media API lists it,
// or nil.
func (db *DB) GetListableMedia(sha256 string) (*MediaRow, error) {
	var m MediaRow
	var at sql.NullFloat64
	err := db.conn.QueryRow(`SELECT sha256, captured_at, COALESCE(mime_type, ''), COALESCE(first_source_path, '') FROM uploads
		 WHERE sha256 = ? AND `+mediaListable, sha256).Scan(&m.SHA256, &at, &m.MimeType, &m.FirstSourcePath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	m.CapturedAt = at.Float64
	return &m, err
}

// FilesSeenWithSHA returns every scanned path that held this content when
// it was last scanned.
func (db *DB) FilesSeenWithSHA(sha256 string) ([]FileSeen, error) {
	rows, err := db.conn.Query(`SELECT path, sha256, mtime, size, last_scanned FROM files_seen WHERE sha256 = ? ORDER BY path`, sha256)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FileSeen
	for rows.Next() {
		var f FileSeen
		if err := rows.Scan(&f.Path, &f.SHA256, &f.Mtime, &f.Size, &f.LastScanned); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
