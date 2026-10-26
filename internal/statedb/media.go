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
	CaptureSource   sql.NullString
	CaptureOffset   sql.NullString
	Lat, Lon        sql.NullFloat64
	Width, Height   sql.NullInt64
}

const mediaRowCols = `sha256, captured_at, COALESCE(mime_type, ''), COALESCE(first_source_path, ''),
	capture_source, capture_offset, gps_lat, gps_lon, width, height`

func scanMediaRow(row interface{ Scan(...any) error }) (MediaRow, error) {
	var m MediaRow
	var at sql.NullFloat64
	err := row.Scan(&m.SHA256, &at, &m.MimeType, &m.FirstSourcePath,
		&m.CaptureSource, &m.CaptureOffset, &m.Lat, &m.Lon, &m.Width, &m.Height)
	m.CapturedAt = at.Float64
	return m, err
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
	q := `SELECT ` + mediaRowCols + ` FROM uploads
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
		m, err := scanMediaRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetListableMedia returns the row for sha256 if the media API lists it,
// or nil.
func (db *DB) GetListableMedia(sha256 string) (*MediaRow, error) {
	m, err := scanMediaRow(db.conn.QueryRow(`SELECT `+mediaRowCols+` FROM uploads WHERE sha256 = ? AND `+mediaListable, sha256))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
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

// MediaMetadata is what RecordMediaMetadata writes. A nil or empty field is
// stored as NULL: unknown.
type MediaMetadata struct {
	// CapturedAt, if set, replaces captured_at; nil keeps it.
	CapturedAt    *float64
	Source        string
	Offset        string
	Lat, Lon      *float64
	Width, Height *int
}

// RecordMediaMetadata stores a row's metadata, once: only while the row has
// none (metadata_read_at IS NULL), so the first copy of some content to be
// read decides, and a backfill never reads a row twice. Reports whether it
// wrote.
func (db *DB) RecordMediaMetadata(sha256 string, m MediaMetadata, now float64) (bool, error) {
	res, err := db.conn.Exec(`UPDATE uploads SET captured_at = COALESCE(?, captured_at),
		 capture_source = ?, capture_offset = ?, gps_lat = ?, gps_lon = ?, width = ?, height = ?, metadata_read_at = ?
		 WHERE sha256 = ? AND metadata_read_at IS NULL`,
		m.CapturedAt, nullIfEmpty(m.Source), nullIfEmpty(m.Offset), m.Lat, m.Lon, m.Width, m.Height, now, sha256)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SHA256sWithoutMetadata returns the rows a scan has never read metadata
// for: rows from before it was recorded, for the backfill.
func (db *DB) SHA256sWithoutMetadata() (map[string]bool, error) {
	rows, err := db.conn.Query(`SELECT sha256 FROM uploads WHERE metadata_read_at IS NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = true
	}
	return out, rows.Err()
}
