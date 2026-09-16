package statedb

// MissingSweepRow is one ledger entry a missing-file sweep looks at.
type MissingSweepRow struct {
	SHA256  string
	Path    string
	Flagged bool // already flagged by an earlier scan
}

// MarkMissing flags an entry whose file a scan found absent. missing_since
// keeps the FIRST sighting; missing_checked_at records this one.
func (db *DB) MarkMissing(sha256 string, now float64) error {
	_, err := db.conn.Exec(`UPDATE uploads SET missing_since=COALESCE(missing_since, ?), missing_checked_at=? WHERE sha256=?`, now, now, sha256)
	return err
}

// ClearMissing removes every missing-file mark -- the file is back, or was
// found at a new path.
func (db *DB) ClearMissing(sha256 string) error {
	_, err := db.conn.Exec(`UPDATE uploads SET missing_since=NULL, missing_checked_at=NULL, missing_confirmed_at=NULL WHERE sha256=?`, sha256)
	return err
}

// ConfirmMissing marks a flagged entry as confirmed missing after a full scan.
func (db *DB) ConfirmMissing(sha256 string, now float64) error {
	_, err := db.conn.Exec(`UPDATE uploads SET missing_confirmed_at=? WHERE sha256=? AND missing_since IS NOT NULL`, now, sha256)
	return err
}

// UnconfirmMissing withdraws a confirmation but keeps the flag: a later
// check could no longer rule out a move, so recheck --missing must not act
// on the entry until the next pass decides it again.
func (db *DB) UnconfirmMissing(sha256 string) error {
	_, err := db.conn.Exec(`UPDATE uploads SET missing_confirmed_at=NULL WHERE sha256=?`, sha256)
	return err
}

// MissingCandidate is a flagged entry, confirmed or not.
type MissingCandidate struct {
	SHA256    string
	Path      string
	Size      int64
	Confirmed bool
}

// MissingFlagged returns every entry a scan has flagged as missing,
// confirmed or not.
func (db *DB) MissingFlagged() ([]MissingCandidate, error) {
	rows, err := db.conn.Query(`SELECT sha256, first_source_path, COALESCE(size, 0), missing_confirmed_at IS NOT NULL FROM uploads
		WHERE missing_since IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissingCandidate
	for rows.Next() {
		var c MissingCandidate
		if err := rows.Scan(&c.SHA256, &c.Path, &c.Size, &c.Confirmed); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConfirmedMissingRow is one confirmed-missing entry, for reports and
// gpsync recheck --missing.
type ConfirmedMissingRow struct {
	SHA256 string
	Path   string
	Status string
	Size   int64
}

// ConfirmedMissing returns every confirmed-missing entry, ordered by path.
func (db *DB) ConfirmedMissing() ([]ConfirmedMissingRow, error) {
	rows, err := db.conn.Query(`SELECT sha256, first_source_path, status, COALESCE(size, 0) FROM uploads
		WHERE missing_confirmed_at IS NOT NULL ORDER BY first_source_path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConfirmedMissingRow
	for rows.Next() {
		var r ConfirmedMissingRow
		if err := rows.Scan(&r.SHA256, &r.Path, &r.Status, &r.Size); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MissingCounts summarises both stages, for reports and the dashboard.
type MissingCounts struct {
	Flagged        int // flagged, awaiting a full scan
	Confirmed      int
	ConfirmedBytes int64
}

func (db *DB) MissingCounts() (MissingCounts, error) {
	var c MissingCounts
	err := db.conn.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN missing_since IS NOT NULL AND missing_confirmed_at IS NULL THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN missing_confirmed_at IS NOT NULL THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN missing_confirmed_at IS NOT NULL THEN size ELSE 0 END), 0)
		FROM uploads`).Scan(&c.Flagged, &c.Confirmed, &c.ConfirmedBytes)
	return c, err
}
